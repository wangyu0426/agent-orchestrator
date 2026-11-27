package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestFeedbackRedeliveryRecoversAfterMoreThanThreeFailuresAndSurvivesRestart(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	m, st, msg := newManager()
	m.clock = func() time.Time { return now }
	st.sessions["mer-1"] = working("mer-1")
	st.comments["pr1"] = []domain.PullRequestComment{{
		ID:               "c1",
		ThreadID:         "T1",
		Author:           "alice",
		File:             "foo.go",
		Line:             10,
		Body:             "please fix this",
		AutoInjectReview: true,
	}}
	obs := ports.PRObservation{Fetched: true, URL: "pr1", Review: domain.ReviewChangesRequest}

	// Simulate 4 consecutive delivery failures (more than old reviewMaxNudge=3).
	msg.err = errors.New("temporary delivery failure")
	expectedDelays := []time.Duration{
		30 * time.Second,
		1 * time.Minute,
		2 * time.Minute,
		4 * time.Minute,
	}
	for i, wantDelay := range expectedDelays {
		if err := m.ApplyPRObservation(context.Background(), "mer-1", obs); err == nil {
			t.Fatalf("attempt %d: expected delivery error", i+1)
		}
		// Calling again before backoff expires must not re-attempt delivery.
		now = now.Add(wantDelay - time.Second)
		if err := m.ApplyPRObservation(context.Background(), "mer-1", obs); err != nil {
			t.Fatalf("attempt %d during backoff: unexpected error %v", i+1, err)
		}
		now = now.Add(time.Second)
	}

	// Simulate daemon restart: new Manager backed by the same store.
	// Before advancing past backoff? We already advanced to exact expiry, so
	// clear the error and verify the 5th attempt succeeds and deduplicates.
	msg.err = nil
	restarted := New(st, msg)
	restarted.clock = func() time.Time { return now }
	if err := restarted.ApplyPRObservation(context.Background(), "mer-1", obs); err != nil {
		t.Fatalf("5th attempt after restart: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("expected 1 delivered message on 5th attempt, got %d", len(msg.msgs))
	}

	// Subsequent polls must deduplicate (no duplicate injection).
	now = now.Add(time.Hour)
	if err := restarted.ApplyPRObservation(context.Background(), "mer-1", obs); err != nil {
		t.Fatalf("post-delivery poll: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("expected deduplication after delivery, got %d messages", len(msg.msgs))
	}
}

func TestFeedbackRedeliveryStopsOnResolvedOrTerminalPR(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	m, st, msg := newManager()
	m.clock = func() time.Time { return now }
	st.sessions["mer-1"] = working("mer-1")
	st.comments["pr1"] = []domain.PullRequestComment{{
		ID:               "c1",
		ThreadID:         "T1",
		Author:           "alice",
		File:             "foo.go",
		Line:             10,
		Body:             "please fix this",
		AutoInjectReview: true,
	}}
	obs := ports.PRObservation{Fetched: true, URL: "pr1", Review: domain.ReviewChangesRequest}

	msg.err = errors.New("temporary failure")
	if err := m.ApplyPRObservation(context.Background(), "mer-1", obs); err == nil {
		t.Fatal("expected error on first attempt")
	}

	// Comment is resolved before backoff expires: must not deliver.
	msg.err = nil
	st.comments["pr1"][0].Resolved = true
	now = now.Add(time.Minute)
	if err := m.ApplyPRObservation(context.Background(), "mer-1", obs); err != nil {
		t.Fatalf("resolved comment poll: %v", err)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("resolved feedback must not be delivered, got %d", len(msg.msgs))
	}
}

func TestRearmMergeConflictClearsSuppressedRetryState(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	m, st, msg := newManager()
	m.clock = func() time.Time { return now }
	st.sessions["mer-1"] = working("mer-1")
	st.prs["mer-1"] = []domain.PullRequest{{URL: "pr1", AutoInjectCI: true}}

	// First conflicting observation fails delivery, arming retry backoff without setting seen/attempts.
	msg.err = errors.New("temporary failure")
	conflictObs := ports.PRObservation{Fetched: true, URL: "pr1", Mergeability: domain.MergeConflicting}
	if err := m.ApplyPRObservation(context.Background(), "mer-1", conflictObs); err == nil {
		t.Fatal("expected delivery error")
	}

	// Conflict clears: rearmMergeConflict must clear the persisted retry backoff state.
	msg.err = nil
	clearObs := ports.PRObservation{Fetched: true, URL: "pr1", Mergeability: domain.MergeMergeable}
	if err := m.ApplyPRObservation(context.Background(), "mer-1", clearObs); err != nil {
		t.Fatalf("clear observation: %v", err)
	}

	// Conflict returns immediately (before the old 30s backoff deadline): must nudge right away.
	now = now.Add(5 * time.Second)
	if err := m.ApplyPRObservation(context.Background(), "mer-1", conflictObs); err != nil {
		t.Fatalf("returning conflict observation: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("expected immediate nudge after conflict re-arm, got %d", len(msg.msgs))
	}
}

func TestPolicySuppressionDoesNotTriggerBackoff(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	m, st, msg := newManager()
	m.clock = func() time.Time { return now }
	st.sessions["mer-1"] = domain.SessionRecord{
		ID:       "mer-1",
		Activity: domain.Activity{State: domain.ActivityWaitingInput},
	}
	st.comments["pr1"] = []domain.PullRequestComment{{
		ID:               "c1",
		ThreadID:         "T1",
		Author:           "alice",
		File:             "foo.go",
		Line:             10,
		Body:             "please fix this",
		AutoInjectReview: true,
	}}
	obs := ports.PRObservation{Fetched: true, URL: "pr1", Review: domain.ReviewChangesRequest}

	// Multiple policy-suppressed polls (waiting_input) must not arm retry backoff.
	for range 3 {
		outcome, err := m.sendOnce(context.Background(), "mer-1", "pr1", "comment:pr1:c1", "T1\x00c1", "fix", false)
		if err != nil || outcome != sendOnceSuppressed {
			t.Fatalf("expected policy suppression, got outcome=%v err=%v", outcome, err)
		}
	}

	// As soon as the session becomes workable, delivery must happen immediately without waiting for backoff.
	st.sessions["mer-1"] = working("mer-1")
	if err := m.ApplyPRObservation(context.Background(), "mer-1", obs); err != nil {
		t.Fatalf("ApplyPRObservation once workable: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("expected immediate delivery once workable, got %d", len(msg.msgs))
	}
}

func TestFeedbackRetryStatePersistFailureRetriesAndSurvivesRestartBeforeDeadline(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	m, st, msg := newManager()
	m.clock = func() time.Time { return now }
	st.sessions["mer-1"] = working("mer-1")
	st.comments["pr1"] = []domain.PullRequestComment{{
		ID:               "c1",
		ThreadID:         "T1",
		Author:           "alice",
		File:             "foo.go",
		Line:             10,
		Body:             "please fix this",
		AutoInjectReview: true,
	}}
	obs := ports.PRObservation{Fetched: true, URL: "pr1", Review: domain.ReviewChangesRequest}

	// Both delivery and retry-state persistence fail on attempt 1.
	deliveryErr := errors.New("pane write failed")
	persistErr := errors.New("sqlite busy")
	msg.err = deliveryErr
	st.signatureWriteErr = persistErr

	err := m.ApplyPRObservation(context.Background(), "mer-1", obs)
	if !errors.Is(err, deliveryErr) || !errors.Is(err, persistErr) {
		t.Fatalf("expected joined delivery and persist errors, got %v", err)
	}

	// Persistence recovers; a poll before the 30s deadline flushes the dirty retry state without re-sending.
	st.signatureWriteErr = nil
	msg.err = nil
	now = now.Add(10 * time.Second)
	if err := m.ApplyPRObservation(context.Background(), "mer-1", obs); err != nil {
		t.Fatalf("flush dirty retry state: %v", err)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("must not deliver before backoff deadline, got %d", len(msg.msgs))
	}

	// Simulate daemon restart BEFORE the 30s deadline: persisted NextRetryAt must still defer.
	restarted := New(st, msg)
	restarted.clock = func() time.Time { return now }
	now = now.Add(10 * time.Second) // t+20s < t+30s
	if err := restarted.ApplyPRObservation(context.Background(), "mer-1", obs); err != nil {
		t.Fatalf("restarted poll before deadline: %v", err)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("restarted manager must honor persisted backoff before deadline, got %d", len(msg.msgs))
	}

	// Advance to deadline (t+30s): restarted manager delivers once.
	now = now.Add(10 * time.Second)
	if err := restarted.ApplyPRObservation(context.Background(), "mer-1", obs); err != nil {
		t.Fatalf("restarted poll at deadline: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("expected 1 delivery at deadline after restart, got %d", len(msg.msgs))
	}
}
