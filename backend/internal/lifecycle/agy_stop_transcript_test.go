package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestAgyStopTranscriptPersistsTurnDeduplicatesAndGuardsRecovery(t *testing.T) {
	ctx := context.Background()
	st := sqlitetest.MustOpen(t)

	now := time.Now().UTC().Truncate(time.Second)
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-1", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	rec, err := st.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "proj-1",
		Kind:      domain.KindWorker,
		Harness:   domain.HarnessAgy,
		Mode:      domain.SessionModeTUI,
		CreatedAt: now.Add(-5 * time.Minute),
		UpdatedAt: now.Add(-5 * time.Minute),
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-5 * time.Minute)},
		Metadata: domain.SessionMetadata{
			RuntimeLaunchID:        "launch-1",
			AgentSessionID:         "conv-1",
			AgentSessionIDLaunchID: "launch-1",
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	mgr := New(st, nil)
	stopSignal := ports.ActivitySignal{
		Valid:                 true,
		State:                 domain.ActivityIdle,
		Event:                 "stop",
		Timestamp:             now,
		LaunchID:              "launch-1",
		AgentSessionID:        "conv-1",
		ExpectedHarness:       domain.HarnessAgy,
		TranscriptPath:        "/home/test/.gemini/antigravity-cli/brain/conv-1/.system_generated/logs/transcript.jsonl",
		TurnUserRequest:       "implement feature X",
		FinalAssistantMessage: "Implemented feature X and verified tests.",
		ProviderTurnID:        "step:6",
	}

	// First Stop delivery persists native_transcript_path and terminal turn.
	if err := mgr.ApplyActivitySignal(ctx, rec.ID, stopSignal); err != nil {
		t.Fatalf("first stop signal: %v", err)
	}
	// Duplicate Stop delivery / replay must not duplicate the terminal turn.
	if err := mgr.ApplyActivitySignal(ctx, rec.ID, stopSignal); err != nil {
		t.Fatalf("duplicate stop signal: %v", err)
	}

	gotRec, ok, err := st.GetSession(ctx, rec.ID)
	if err != nil || !ok {
		t.Fatalf("get session: ok=%v err=%v", ok, err)
	}
	if gotRec.Metadata.NativeTranscriptPath != stopSignal.TranscriptPath {
		t.Fatalf("NativeTranscriptPath = %q, want %q", gotRec.Metadata.NativeTranscriptPath, stopSignal.TranscriptPath)
	}
	if gotRec.Activity.State != domain.ActivityIdle {
		t.Fatalf("Activity.State = %q, want idle", gotRec.Activity.State)
	}
	if gotRec.FirstSignalAt.IsZero() {
		t.Fatalf("expected FirstSignalAt to be set after Stop hook")
	}

	turns, err := st.ListTerminalTurns(ctx, rec.ID)
	if err != nil {
		t.Fatalf("list terminal turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want 1 (no duplicates)", len(turns))
	}
	if turns[0].UserRequest != "implement feature X" || turns[0].AgentResponse != "Implemented feature X and verified tests." {
		t.Fatalf("unexpected turn: %+v", turns[0])
	}

	// Simulate recovery/relaunch: RuntimeLaunchID advances and FirstSignalAt resets.
	gotRec.Metadata.RuntimeLaunchID = "launch-2"
	gotRec.FirstSignalAt = time.Time{}
	if err := st.UpdateSession(ctx, gotRec); err != nil {
		t.Fatalf("update session for recovery: %v", err)
	}

	// Stale Stop from launch-1 must be ignored and must not restore FirstSignalAt
	// or attach an old turn to launch-2.
	stale := stopSignal
	stale.FinalAssistantMessage = "old launch turn"
	if err := mgr.ApplyActivitySignal(ctx, rec.ID, stale); err != nil {
		t.Fatalf("stale stop signal: %v", err)
	}
	afterStale, ok, err := st.GetSession(ctx, rec.ID)
	if err != nil || !ok {
		t.Fatalf("get session after stale: ok=%v err=%v", ok, err)
	}
	if !afterStale.FirstSignalAt.IsZero() {
		t.Fatalf("stale launch-1 signal must not set FirstSignalAt (legitimate no_signal window)")
	}
	turns, err = st.ListTerminalTurns(ctx, rec.ID)
	if err != nil {
		t.Fatalf("list turns after stale: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("stale stop appended a turn: len=%d", len(turns))
	}

	// First fresh post-recovery hook (pre-invocation) restores activity health.
	if err := mgr.ApplyActivitySignal(ctx, rec.ID, ports.ActivitySignal{
		Valid:           true,
		State:           domain.ActivityActive,
		Event:           "pre-invocation",
		Timestamp:       now.Add(2 * time.Minute),
		LaunchID:        "launch-2",
		AgentSessionID:  "conv-1",
		ExpectedHarness: domain.HarnessAgy,
	}); err != nil {
		t.Fatalf("post-recovery pre-invocation: %v", err)
	}
	recovered, ok, err := st.GetSession(ctx, rec.ID)
	if err != nil || !ok {
		t.Fatalf("get recovered session: ok=%v err=%v", ok, err)
	}
	if recovered.FirstSignalAt.IsZero() {
		t.Fatalf("expected FirstSignalAt to be restored on first fresh post-recovery hook")
	}
	if recovered.Activity.State != domain.ActivityActive {
		t.Fatalf("Activity.State = %q, want active", recovered.Activity.State)
	}
}
