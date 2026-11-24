package daemon

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

type quotaChatStub struct {
	failedAt      time.Time
	switches      []domain.ProviderSwitchRequest
	parks         int
	beforeReserve func(context.Context) error
}

func (c *quotaChatStub) QuotaFailure(context.Context, domain.SessionRecord) (domain.ConversationTurn, error) {
	return domain.ConversationTurn{ID: "failed-turn", CompletedAt: &c.failedAt}, nil
}

func (*quotaChatStub) SupportsChat(domain.AgentHarness) bool { return true }
func (*quotaChatStub) QuotaTargetReady(context.Context, domain.SessionID, domain.AgentHarness) bool {
	return true
}

func (c *quotaChatStub) SwitchQuotaProviderAndSend(ctx context.Context, _ domain.SessionID, req domain.ProviderSwitchRequest, _ ports.ChatUserMessage, reserve func(context.Context) error) (domain.ConversationTurn, bool, error) {
	if c.beforeReserve != nil {
		if err := c.beforeReserve(ctx); err != nil {
			return domain.ConversationTurn{}, false, err
		}
	}
	if err := reserve(ctx); err != nil {
		return domain.ConversationTurn{}, false, err
	}
	c.switches = append(c.switches, req)
	return domain.ConversationTurn{}, true, nil
}

func (c *quotaChatStub) ParkQuotaChat(context.Context, domain.SessionRecord, string) (bool, error) {
	c.parks++
	return true, nil
}

func TestWorkerQuotaReconcilesRemainingRoutesInAdmissionOrder(t *testing.T) {
	codexA := workerquota.Target{Harness: domain.HarnessCodex, Model: "codex-a", Effort: "high"}
	agy := workerquota.Target{Harness: domain.HarnessAgy, Model: "gemini"}
	codexB := workerquota.Target{Harness: domain.HarnessCodex, Model: "codex-b", Effort: "low"}
	claude := workerquota.Target{Harness: domain.HarnessClaudeCode, Model: "claude"}
	for _, tc := range []struct {
		name      string
		admitted  workerquota.Target
		fallbacks []workerquota.Target
		remaining []workerquota.Target
		want      workerquota.Target
	}{
		{"primary before duplicate harness", codexA, []workerquota.Target{agy, codexB}, []workerquota.Target{agy, codexB}, agy},
		{"initial fallback keeps forward order", codexB, []workerquota.Target{codexA, agy, codexB, claude}, []workerquota.Target{claude}, claude},
		{"last fallback does not move backwards", codexB, []workerquota.Target{agy, codexB}, []workerquota.Target{}, workerquota.Target{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store := sqlitetest.MustOpen(t)
			now := time.Now().UTC()
			record, err := store.CreateSession(ctx, domain.SessionRecord{
				Kind: domain.KindWorker, Mode: domain.SessionModeChat, Harness: tc.admitted.Harness,
				WorkerQuotaRemaining: tc.remaining, CreatedAt: now, UpdatedAt: now,
				Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
				Metadata: domain.SessionMetadata{Model: tc.admitted.Model, Effort: tc.admitted.Effort},
			})
			if err != nil {
				t.Fatal(err)
			}
			chat := &quotaChatStub{failedAt: now}
			sweep := workerQuotaSweep{
				policy: &workerquota.Policy{Fallbacks: tc.fallbacks},
				store:  store, chat: chat, log: slog.New(slog.DiscardHandler),
			}
			if err := sweep.poll(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.want.Harness == "" {
				if len(chat.switches) != 0 || chat.parks != 1 {
					t.Fatalf("expected only parking, switches=%v parks=%d", chat.switches, chat.parks)
				}
				return
			}
			if len(chat.switches) != 1 || chat.parks != 0 {
				t.Fatalf("expected one switch, switches=%v parks=%d", chat.switches, chat.parks)
			}
			req := chat.switches[0]
			if req.TargetHarness != tc.want.Harness || req.Model != tc.want.Model || req.Effort != tc.want.Effort ||
				req.ExpectedSource == nil || *req.ExpectedSource != record.ControllerOwner() || req.ExpectedQuotaTurnID != "failed-turn" || req.ExpectedSourceModel != tc.admitted.Model {
				t.Fatalf("handoff request = %+v, target want %+v", req, tc.want)
			}
			sessions, err := store.WorkerQuotaSessions(ctx)
			if err != nil || len(sessions) != 1 || sessions[0].Attempt == "" {
				t.Fatalf("durable handoff reservation: %+v, %v", sessions, err)
			}
			if err := sweep.poll(ctx); err != nil || len(chat.switches) != 1 {
				t.Fatalf("second sweep repeated handoff: switches=%v err=%v", chat.switches, err)
			}
		})
	}
}

func TestWorkerQuotaPreservesManualModelChoiceAfterDefaultAdmission(t *testing.T) {
	ctx := t.Context()
	store := sqlitetest.MustOpen(t)
	now := time.Now().UTC()
	fallback := workerquota.Target{Harness: domain.HarnessAgy, Model: "gemini"}
	record, err := store.CreateSession(ctx, domain.SessionRecord{
		Kind: domain.KindWorker, Mode: domain.SessionModeChat, Harness: domain.HarnessCodex,
		WorkerQuotaRemaining: []workerquota.Target{fallback}, CreatedAt: now, UpdatedAt: now,
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := sessionmanager.New(sessionmanager.Deps{Store: store})
	if err := manager.PersistChatModel(ctx, record.ID, "manual-model"); err != nil {
		t.Fatal(err)
	}

	chat := &quotaChatStub{failedAt: now}
	sweep := workerQuotaSweep{
		policy: &workerquota.Policy{}, store: store, chat: chat, log: slog.New(slog.DiscardHandler),
	}
	if err := sweep.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(chat.switches) != 0 || chat.parks != 0 {
		t.Fatalf("manual model choice triggered quota recovery: switches=%v parks=%d", chat.switches, chat.parks)
	}
	sessions, err := store.WorkerQuotaSessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].Attempt != "" {
		t.Fatalf("manual model choice consumed the handoff reservation: %+v, %v", sessions, err)
	}

	if err := manager.PersistChatModel(ctx, record.ID, ""); err != nil {
		t.Fatal(err)
	}
	chat.beforeReserve = func(ctx context.Context) error {
		return manager.PersistChatModel(ctx, record.ID, "manual-model-during-sweep")
	}
	if err := sweep.reconcile(ctx, sessions[0]); err != nil {
		t.Fatal(err)
	}
	if len(chat.switches) != 0 || chat.parks != 0 {
		t.Fatalf("late manual choice triggered quota recovery: switches=%v parks=%d", chat.switches, chat.parks)
	}
	sessions, err = store.WorkerQuotaSessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].Attempt != "" || sessions[0].Reason != "" {
		t.Fatalf("stale reservation changed the handoff or diagnostic: %+v, %v", sessions, err)
	}

	chat.beforeReserve = nil
	if err := manager.PersistChatModel(ctx, record.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := sweep.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(chat.switches) != 1 || chat.switches[0].TargetHarness != fallback.Harness || chat.parks != 0 {
		t.Fatalf("provider default failed to regain quota recovery: switches=%v parks=%d", chat.switches, chat.parks)
	}
}

func TestWorkerQuotaInFlightHandoffParksInsteadOfReinterpretingTUIMode(t *testing.T) {
	ctx := t.Context()
	store := sqlitetest.MustOpen(t)
	now := time.Now().UTC()
	tuiFallback := workerquota.Target{Harness: domain.HarnessAgy, Model: "gemini-4-hydrogen-low", Mode: domain.SessionModeTUI}
	_, err := store.CreateSession(ctx, domain.SessionRecord{
		Kind: domain.KindWorker, Mode: domain.SessionModeChat, Harness: domain.HarnessCodex,
		WorkerQuotaRemaining: []workerquota.Target{tuiFallback}, CreatedAt: now, UpdatedAt: now,
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		Metadata: domain.SessionMetadata{Model: "codex-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	chat := &quotaChatStub{failedAt: now}
	sweep := workerQuotaSweep{
		policy: &workerquota.Policy{Fallbacks: []workerquota.Target{tuiFallback}},
		store:  store, chat: chat, log: slog.New(slog.DiscardHandler),
	}
	if err := sweep.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(chat.switches) != 0 || chat.parks != 1 {
		t.Fatalf("expected safe park without reinterpreting mode:tui as Chat; switches=%v parks=%d", chat.switches, chat.parks)
	}
}
