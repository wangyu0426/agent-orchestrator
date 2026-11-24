package chat_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestWorkerQuotaHandoffKeepsSessionAndStartsContinuation(t *testing.T) {
	h, _, driver := newEditHarness(t, false)
	ctx := t.Context()
	record, _, err := h.st.GetSession(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	record.IssueID = "github:acme/repo#14"
	record.Metadata.WorkspacePath = t.TempDir()
	if err := h.st.UpdateSession(ctx, record); err != nil {
		t.Fatal(err)
	}
	dirty := filepath.Join(record.Metadata.WorkspacePath, "uncommitted.txt")
	if err := os.WriteFile(dirty, []byte("work in progress"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequest{URL: "https://github.com/acme/repo/pull/42", SessionID: testSession, Number: 42, UpdatedAt: h.now()}
	if _, err := h.st.ClaimPR(ctx, pr, nil, nil, nil, nil, ports.ReviewWritePreserve, false); err != nil {
		t.Fatal(err)
	}

	for _, failure := range []domain.ConversationFailureCode{"", domain.ConversationFailureQuotaExhausted} {
		source, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "finish the tracked issue"})
		if err != nil {
			t.Fatal(err)
		}
		h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: source.ProviderTurnID,
			TurnState: domain.TurnStateFailed, Err: errors.New("Usage limit reached"), FailureCode: failure})
		_ = failedTurnSnapshot(t, h, source.ID)
		rec, ok, err := h.st.GetSession(ctx, testSession)
		if err != nil || !ok {
			t.Fatalf("read session: %v, %v", ok, err)
		}
		current, err := h.svc.QuotaFailure(ctx, rec)
		if err != nil || (failure == "" && current.ID != "") || (failure != "" && current.ID != source.ID) {
			t.Fatalf("current quota failure = %+v, %v", current, err)
		}
		if current.ID == "" {
			continue
		}
		owner := rec.ControllerOwner()
		owner.ControllerGeneration = "superseded"
		request := domain.ProviderSwitchRequest{TargetHarness: domain.HarnessAgy, ExpectedSource: &owner, ExpectedQuotaTurnID: current.ID}
		if err := h.svc.SwitchProvider(ctx, rec.ID, request); err == nil {
			t.Fatal("previous controller generation admitted")
		}
		owner = rec.ControllerOwner()
		_, switched, err := h.svc.SwitchProviderAndSend(ctx, rec.ID, request, ports.ChatUserMessage{
			Text: "Continue the current task.", ClientMessageID: "quota-handoff-once", Origin: domain.MessageOriginAutomation,
		})
		if err != nil || !switched {
			t.Fatalf("quota handoff = %v, %v", switched, err)
		}
		got, ok, err := h.st.GetSession(ctx, rec.ID)
		if err != nil || !ok || got.Harness != domain.HarnessAgy || got.ProjectID != rec.ProjectID ||
			got.Metadata.WorkspacePath != rec.Metadata.WorkspacePath || got.IssueID != rec.IssueID {
			t.Fatalf("session changed unexpectedly: %+v, %v", got, err)
		}
		if gotPR, ok, err := h.st.GetPR(ctx, pr.URL); err != nil || !ok || gotPR.SessionID != rec.ID {
			t.Fatalf("PR owner changed: %+v, %v", gotPR, err)
		}
		if data, err := os.ReadFile(dirty); err != nil || string(data) != "work in progress" {
			t.Fatalf("dirty file changed: %q, %v", data, err)
		}

		if !strings.Contains(lastStart(driver).SystemPrompt, "finish the tracked issue") || len(driver.fresh.sentMessages()) != 1 {
			t.Fatalf("target context present = %v, continuation sends = %d", strings.Contains(lastStart(driver).SystemPrompt, "finish the tracked issue"), len(driver.fresh.sentMessages()))
		}
	}
}

func TestWorkerQuotaParksOnlyCurrentQuiescentFailure(t *testing.T) {
	h, conv := settledHibernationHarness(t, "", func() bool { return false })
	ctx := t.Context()
	turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "keep the worktree"})
	if err != nil {
		t.Fatal(err)
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID,
		TurnState: domain.TurnStateFailed, Err: errors.New("Usage limit reached"), FailureCode: domain.ConversationFailureQuotaExhausted})
	_ = failedTurnSnapshot(t, h, turn.ID)
	if err := h.svc.DrainQueued(ctx, testSession); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := h.st.GetSession(ctx, testSession)
	if err != nil || !ok {
		t.Fatalf("read session: %v, %v", ok, err)
	}
	if stopped, err := h.svc.ParkQuotaChat(ctx, rec, "previous-turn"); err != nil || stopped {
		t.Fatalf("stale failure parked: %v, %v", stopped, err)
	}
	conv.backgroundRunning = true
	if stopped, err := h.svc.ParkQuotaChat(ctx, rec, turn.ID); err != nil || stopped {
		t.Fatalf("background work parked: %v, %v", stopped, err)
	}
	conv.backgroundRunning = false
	if stopped, err := h.svc.ParkQuotaChat(ctx, rec, turn.ID); err != nil || !stopped {
		t.Fatalf("park current failure: %v, %v", stopped, err)
	}
	parked, ok, err := h.st.GetSession(ctx, testSession)
	if err != nil || !ok || parked.HibernatedAt == nil || parked.IsTerminated || conv.calls.Load() != 1 ||
		parked.Metadata.WorkspacePath != rec.Metadata.WorkspacePath {
		t.Fatalf("parking changed session ownership: %+v, %v", parked, err)
	}
}
