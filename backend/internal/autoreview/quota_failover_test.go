package autoreview

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	reviewcore "github.com/aoagents/agent-orchestrator/backend/internal/review"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

type quotaTestSource map[workerquota.Target][]workerquota.Window

func (s quotaTestSource) Read(_ context.Context, targets []workerquota.Target) (map[workerquota.Target][]workerquota.Window, error) {
	out := make(map[workerquota.Target][]workerquota.Window, len(targets))
	for _, t := range targets {
		out[t] = s[t]
	}
	return out, nil
}

type recordingOptionsTrigger struct {
	mu    sync.Mutex
	calls []reviewcore.TriggerOptions
}

func (r *recordingOptionsTrigger) TriggerAuto(_ context.Context, _ domain.SessionID, h domain.ReviewerHarness) (reviewcore.TriggerResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, reviewcore.TriggerOptions{Harness: h, Source: domain.ReviewTriggerAuto})
	return reviewcore.TriggerResult{Created: true}, nil
}

func (r *recordingOptionsTrigger) TriggerAutoWithOptions(_ context.Context, _ domain.SessionID, opts reviewcore.TriggerOptions) (reviewcore.TriggerResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, opts)
	return reviewcore.TriggerResult{Created: true}, nil
}

func TestReviewerQuotaFailoverSelectsAlternativeOnCodexExhaustion(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	st := &fakeStore{
		session: domain.SessionRecord{
			ID: "p-1", ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex,
			AutoReviewEnabled: true,
			Activity:          domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-5 * time.Minute)},
		},
		project: domain.ProjectRecord{
			ID: "p", Config: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex}}},
		},
		prs: []domain.PullRequest{{URL: "https://github.com/o/r/pull/1", Number: 1, HeadSHA: "sha-1"}},
	}
	codexTarget := workerquota.Target{Harness: domain.HarnessCodex}
	agyFallback := workerquota.Target{Harness: domain.HarnessAgy, Model: "agy-review-pro", Effort: "high", Mode: domain.SessionModeTUI}
	trig := &recordingOptionsTrigger{}
	coord := New(st, trig, Config{
		Clock: func() time.Time { return now },
		Quota: &workerquota.Policy{
			Role:      workerquota.RoleReviewer,
			Fallbacks: []workerquota.Target{agyFallback},
			Source: quotaTestSource{
				codexTarget: {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), ResetAt: now.Add(2 * time.Hour), Exhausted: true}},
			},
		},
	})

	res, err := coord.EvaluateSession(context.Background(), "p-1")
	if err != nil {
		t.Fatalf("EvaluateSession: %v", err)
	}
	if !res.Triggered {
		t.Fatalf("Triggered = false (reason=%s), want true", res.Reason)
	}
	if len(trig.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(trig.calls))
	}
	got := trig.calls[0]
	if got.Harness != domain.ReviewerAgy || got.Config.Model != "agy-review-pro" || got.Config.Effort != "high" || got.InterfaceMode != domain.ReviewerInterfaceTUI {
		t.Fatalf("trigger opts = %+v, want agy/agy-review-pro/high/tui", got)
	}
}

func TestReviewerQuotaRespectsExplicitSessionOverride(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	st := &fakeStore{
		session: domain.SessionRecord{
			ID: "p-1", ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex,
			ReviewerHarness:   domain.ReviewerCodex,
			AutoReviewEnabled: true,
			Activity:          domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-5 * time.Minute)},
		},
		project: domain.ProjectRecord{ID: "p"},
		prs:     []domain.PullRequest{{URL: "https://github.com/o/r/pull/1", Number: 1, HeadSHA: "sha-1"}},
	}
	trig := &recordingOptionsTrigger{}
	coord := New(st, trig, Config{
		Clock: func() time.Time { return now },
		Quota: &workerquota.Policy{
			Role:      workerquota.RoleReviewer,
			Fallbacks: []workerquota.Target{{Harness: domain.HarnessAgy}},
			Source: quotaTestSource{
				{Harness: domain.HarnessCodex}: {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), ResetAt: now.Add(2 * time.Hour), Exhausted: true}},
			},
		},
	})
	res, err := coord.EvaluateSession(context.Background(), "p-1")
	if err != nil || !res.Triggered || len(trig.calls) != 1 || trig.calls[0].Harness != domain.ReviewerCodex {
		t.Fatalf("explicit override bypassed: res=%+v err=%v calls=%+v", res, err, trig.calls)
	}
}

func TestReviewerQuotaDeduplicatesAcrossHarnessesOnSameHead(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	st := &fakeStore{
		session: domain.SessionRecord{
			ID: "p-1", ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex,
			AutoReviewEnabled: true,
			Activity:          domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-5 * time.Minute)},
		},
		project: domain.ProjectRecord{
			ID: "p", Config: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex}}},
		},
		prs:  []domain.PullRequest{{URL: "https://github.com/o/r/pull/1", Number: 1, HeadSHA: "sha-1"}},
		runs: []domain.ReviewRun{{PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha-1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved}},
	}
	trig := &recordingOptionsTrigger{}
	coord := New(st, trig, Config{
		Clock: func() time.Time { return now },
		Quota: &workerquota.Policy{
			Role:      workerquota.RoleReviewer,
			Fallbacks: []workerquota.Target{{Harness: domain.HarnessAgy}},
			Source: quotaTestSource{
				{Harness: domain.HarnessCodex}: {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), ResetAt: now.Add(2 * time.Hour), Exhausted: true}},
			},
		},
	})
	res, err := coord.EvaluateSession(context.Background(), "p-1")
	if err != nil {
		t.Fatalf("EvaluateSession: %v", err)
	}
	if res.Triggered || res.Reason != "already_approved" || len(trig.calls) != 0 {
		t.Fatalf("expected deduplication on already approved HEAD, got res=%+v calls=%d", res, len(trig.calls))
	}
}

func TestReviewerQuotaAllExhaustedDefersWithResetDeadlineAndRecovers(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(30 * time.Minute)
	st := &fakeStore{
		session: domain.SessionRecord{
			ID: "p-1", ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex,
			AutoReviewEnabled: true,
			Activity:          domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-5 * time.Minute)},
		},
		project: domain.ProjectRecord{
			ID: "p", Config: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex}}},
		},
		prs: []domain.PullRequest{{URL: "https://github.com/o/r/pull/1", Number: 1, HeadSHA: "sha-1"}},
	}
	trig := &recordingOptionsTrigger{}
	coord := New(st, trig, Config{
		Clock: func() time.Time { return now },
		Quota: &workerquota.Policy{
			Role:      workerquota.RoleReviewer,
			Fallbacks: []workerquota.Target{{Harness: domain.HarnessAgy}},
			Source: quotaTestSource{
				{Harness: domain.HarnessCodex}: {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(2 * time.Hour), ResetAt: resetAt, Exhausted: true}},
				{Harness: domain.HarnessAgy}:   {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(2 * time.Hour), ResetAt: now.Add(time.Hour), Exhausted: true}},
			},
		},
	})

	res, err := coord.EvaluateSession(context.Background(), "p-1")
	if err != nil {
		t.Fatalf("EvaluateSession: %v", err)
	}
	if res.Triggered || res.Reason != "reviewer_quota_deferred" || !res.RetryAt.Equal(resetAt) {
		t.Fatalf("expected deferral until %v, got %+v", resetAt, res)
	}
	if len(trig.calls) != 0 {
		t.Fatalf("deferred review must never trigger or approve, got %d calls", len(trig.calls))
	}

	// Advance clock past reset: primary Codex recovers and triggers!
	now = resetAt.Add(time.Second)
	res, err = coord.EvaluateSession(context.Background(), "p-1")
	if err != nil {
		t.Fatalf("EvaluateSession after reset: %v", err)
	}
	if !res.Triggered || len(trig.calls) != 1 || trig.calls[0].Harness != domain.ReviewerCodex {
		t.Fatalf("expected primary Codex recovery after reset, got res=%+v calls=%+v", res, trig.calls)
	}
}
