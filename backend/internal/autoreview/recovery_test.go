package autoreview

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestAutoReviewStateDrivenBackoffAndRestartRecovery(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		session: domain.SessionRecord{
			ID:                "s1",
			ProjectID:         "p1",
			Kind:              domain.KindWorker,
			Harness:           domain.HarnessCodex,
			AutoReviewEnabled: true,
			Activity:          domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-5 * time.Minute)},
		},
		project: domain.ProjectRecord{ID: "p1"},
		prs:     []domain.PullRequest{{URL: "pr1", Number: 1, HeadSHA: "sha1"}},
	}

	// Simulate 5 consecutive transient failures on the same HEAD, verifying
	// bounded exponential backoff (1m, 2m, 4m, 8m, 16m) and recovery on attempt 6,
	// including across a simulated daemon restart (new Coordinator instance).
	expectedDelays := []time.Duration{
		1 * time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		16 * time.Minute,
	}
	for i, wantDelay := range expectedDelays {
		attempt := i + 1
		store.runs = append(store.runs, domain.ReviewRun{
			ID:            fmt.Sprintf("fail-%d", attempt),
			PRURL:         "pr1",
			TargetSHA:     "sha1",
			Harness:       domain.ReviewerCodex,
			TriggerSource: domain.ReviewTriggerAuto,
			Status:        domain.ReviewRunFailed,
			Body:          fmt.Sprintf("transient timeout %d", attempt),
			CreatedAt:     now,
		})

		// Fresh Coordinator simulates daemon restart reading persisted ReviewRun rows.
		trig := &fakeTrigger{}
		coord := New(store, trig, Config{Clock: func() time.Time { return now }})

		// Before backoff expires: must defer with exact RetryAt and AttemptCount.
		now = now.Add(wantDelay - time.Second)
		res, err := coord.EvaluateSession(context.Background(), "s1")
		if err != nil {
			t.Fatalf("attempt %d before backoff: %v", attempt, err)
		}
		if res.Triggered || res.Reason != "auto_review_retry_backoff" || res.AttemptCount != attempt || !res.RetryAt.Equal(now.Add(time.Second)) {
			t.Fatalf("attempt %d before backoff = %+v, want backoff reason with attempt=%d", attempt, res, attempt)
		}

		// At backoff expiry: must recover and trigger.
		now = now.Add(time.Second)
		res, err = coord.EvaluateSession(context.Background(), "s1")
		if err != nil {
			t.Fatalf("attempt %d at backoff expiry: %v", attempt, err)
		}
		if !res.Triggered || trig.calls != 1 {
			t.Fatalf("attempt %d at backoff expiry = %+v calls=%d, want triggered", attempt, res, trig.calls)
		}
	}
}

func TestAutoReviewFourPlusLegitimateReviewFixCycles(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		session: domain.SessionRecord{
			ID:                "s1",
			ProjectID:         "p1",
			Kind:              domain.KindWorker,
			Harness:           domain.HarnessCodex,
			AutoReviewEnabled: true,
			Activity:          domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-5 * time.Minute)},
		},
		project: domain.ProjectRecord{ID: "p1"},
	}

	for cycle := 1; cycle <= 5; cycle++ {
		sha := fmt.Sprintf("sha-%d", cycle)
		store.prs = []domain.PullRequest{{URL: "pr1", Number: 1, HeadSHA: sha}}
		trig := &fakeTrigger{}
		coord := New(store, trig, Config{Clock: func() time.Time { return now }})

		res, err := coord.EvaluateSession(context.Background(), "s1")
		if err != nil || !res.Triggered || trig.calls != 1 {
			t.Fatalf("cycle %d (%s) did not trigger: res=%+v err=%v calls=%d", cycle, sha, res, err, trig.calls)
		}

		// Reviewer requests changes on this SHA: same SHA must not re-trigger.
		store.runs = append(store.runs, domain.ReviewRun{
			ID:            fmt.Sprintf("run-%d", cycle),
			PRURL:         "pr1",
			TargetSHA:     sha,
			Harness:       domain.ReviewerCodex,
			TriggerSource: domain.ReviewTriggerAuto,
			Status:        domain.ReviewRunComplete,
			Verdict:       domain.VerdictChangesRequested,
			CreatedAt:     now,
		})
		res, err = coord.EvaluateSession(context.Background(), "s1")
		if err != nil || res.Triggered || res.Reason != "changes_requested_same_sha" {
			t.Fatalf("cycle %d same SHA after changes_requested = %+v err=%v", cycle, res, err)
		}
		now = now.Add(2 * time.Minute)
	}
}

func TestAutoReviewPermanentFailureBlocksWithActionableDetail(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for _, body := range []string{
		"reviewer preflight: invalid API key",
		domain.ReviewRunAuthRequiredBody,
		domain.ReviewRunProviderKeyMissingBody,
	} {
		t.Run(body, func(t *testing.T) {
			store := &fakeStore{
				session: domain.SessionRecord{
					ID:                "s1",
					ProjectID:         "p1",
					Kind:              domain.KindWorker,
					Harness:           domain.HarnessCodex,
					AutoReviewEnabled: true,
					Activity:          domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-5 * time.Minute)},
				},
				project: domain.ProjectRecord{ID: "p1"},
				prs:     []domain.PullRequest{{URL: "pr1", Number: 1, HeadSHA: "sha1"}},
				runs: []domain.ReviewRun{{
					ID:            "perm-1",
					PRURL:         "pr1",
					TargetSHA:     "sha1",
					Harness:       domain.ReviewerCodex,
					TriggerSource: domain.ReviewTriggerAuto,
					Status:        domain.ReviewRunFailed,
					Body:          body,
					CreatedAt:     now.Add(-2 * time.Hour),
				}},
			}
			trig := &fakeTrigger{}
			coord := New(store, trig, Config{Clock: func() time.Time { return now }})

			res, err := coord.EvaluateSession(context.Background(), "s1")
			if err != nil {
				t.Fatal(err)
			}
			if res.Triggered || res.Reason != "permanent_review_failure" || res.Detail != body || trig.calls != 0 {
				t.Fatalf("permanent failure = %+v calls=%d, want permanent_review_failure with detail=%q", res, trig.calls, body)
			}
		})
	}
}
