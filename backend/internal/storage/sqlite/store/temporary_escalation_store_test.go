package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func testEscalationRecord(session domain.SessionID, now time.Time) *domain.TemporaryEscalation {
	return &domain.TemporaryEscalation{
		ID:               "esc-1",
		SessionID:        session,
		ConversationID:   "conv-1",
		WorkItemID:       "turn-1",
		GenerationBefore: "gen-1",
		Mode:             domain.SessionModeChat,
		Original: domain.ModelSnapshot{
			Harness: domain.HarnessCodex, Model: "mai-code-2", Effort: "medium",
			ConversationModel: "mai-code-2", PermissionMode: domain.PermissionModeAuto,
		},
		TargetHarness: domain.HarnessClaudeCode,
		TargetModel:   "sol-6.2",
		State:         domain.TemporaryEscalationPending,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func TestTemporaryEscalationStoreRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "esc-store")
	session, err := s.CreateSession(ctx, sampleRecord("esc-store"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)

	if _, found, err := s.GetEscalation(ctx, session.ID); err != nil || found {
		t.Fatalf("GetEscalation() before create = found=%v err=%v, want false, nil", found, err)
	}
	esc := testEscalationRecord(session.ID, now)
	if err := s.CreateEscalation(ctx, esc); err != nil {
		t.Fatalf("CreateEscalation() = %v", err)
	}
	// A second active escalation for the same session is refused.
	if err := s.CreateEscalation(ctx, testEscalationRecord(session.ID, now)); err == nil {
		t.Error("second CreateEscalation() = nil, want error")
	}
	got, found, err := s.GetEscalation(ctx, session.ID)
	if err != nil || !found {
		t.Fatalf("GetEscalation() = found=%v err=%v", found, err)
	}
	if got.Original.Model != "mai-code-2" || got.TargetModel != "sol-6.2" || got.WorkItemID != "turn-1" {
		t.Fatalf("GetEscalation() = %+v", got)
	}

	// CAS: a stale from-state does not move the record.
	moved := *got
	if err := moved.MarkDone("rpt-1", now.Add(time.Minute)); err != nil {
		t.Fatalf("MarkDone() = %v", err)
	}
	if ok, err := s.UpdateEscalation(ctx, &moved, domain.TemporaryEscalationReverting); err != nil || ok {
		t.Fatalf("UpdateEscalation(wrong from) = %v, %v, want false, nil", ok, err)
	}
	if ok, err := s.UpdateEscalation(ctx, &moved, domain.TemporaryEscalationPending); err != nil || !ok {
		t.Fatalf("UpdateEscalation() = %v, %v, want true, nil", ok, err)
	}
	active, err := s.ListActiveEscalations(ctx)
	if err != nil || len(active) != 1 || active[0].State != domain.TemporaryEscalationAwaitingIdle {
		t.Fatalf("ListActiveEscalations() = %v, %v, want one awaiting_idle", active, err)
	}

	// Terminal records leave the active list and free the slot.
	if err := moved.BeginRevert(now.Add(2 * time.Minute)); err != nil {
		t.Fatalf("BeginRevert() = %v", err)
	}
	if ok, err := s.UpdateEscalation(ctx, &moved, domain.TemporaryEscalationAwaitingIdle); err != nil || !ok {
		t.Fatalf("UpdateEscalation() to reverting = %v, %v", ok, err)
	}
	if err := moved.MarkReverted(now.Add(3 * time.Minute)); err != nil {
		t.Fatalf("MarkReverted() = %v", err)
	}
	if ok, err := s.UpdateEscalation(ctx, &moved, domain.TemporaryEscalationReverting); err != nil || !ok {
		t.Fatalf("UpdateEscalation() to reverted = %v, %v", ok, err)
	}
	if active, err := s.ListActiveEscalations(ctx); err != nil || len(active) != 0 {
		t.Fatalf("ListActiveEscalations() after revert = %v, %v, want empty", active, err)
	}
	next := testEscalationRecord(session.ID, now.Add(4*time.Minute))
	next.ID = "esc-2"
	if err := s.CreateEscalation(ctx, next); err != nil {
		t.Fatalf("CreateEscalation() after terminal = %v", err)
	}
	if err := s.DeleteEscalation(ctx, session.ID); err != nil {
		t.Fatalf("DeleteEscalation() = %v", err)
	}
	if _, found, err := s.GetEscalation(ctx, session.ID); err != nil || found {
		t.Fatalf("GetEscalation() after delete = found=%v err=%v", found, err)
	}
}
