package daemon

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

func TestOrchestratorInFlightQuotaFailureParksWithoutSwitching(t *testing.T) {
	ctx := t.Context()
	store := sqlitetest.MustOpen(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	fallback := workerquota.Target{Harness: domain.HarnessClaudeCode, Model: "claude-opus"}
	if _, err := store.CreateSession(ctx, domain.SessionRecord{
		Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat, Harness: domain.HarnessCodex,
		WorkerQuotaRemaining: []workerquota.Target{fallback}, CreatedAt: now, UpdatedAt: now,
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		Metadata: domain.SessionMetadata{Model: "codex-orch"},
	}); err != nil {
		t.Fatal(err)
	}

	chat := &quotaChatStub{failedAt: now}
	sweep := workerQuotaSweep{
		policy: &workerquota.Policy{Role: workerquota.RoleOrchestrator},
		store:  store,
		chat:   chat,
		log:    slog.New(slog.DiscardHandler),
	}
	if err := sweep.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(chat.switches) != 0 {
		t.Fatalf("orchestrator must never auto-switch in-flight, got switches=%v", chat.switches)
	}
	if chat.parks != 1 {
		t.Fatalf("parks = %d, want 1", chat.parks)
	}
	sessions, err := store.WorkerQuotaSessions(ctx)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("WorkerQuotaSessions = %+v, err=%v", sessions, err)
	}
	if !strings.Contains(sessions[0].Reason, "orchestrator in-flight handoff unsupported") {
		t.Fatalf("reason = %q, want orchestrator parking diagnostic", sessions[0].Reason)
	}
}
