package autoreview

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

func TestReviewerQuotaDurableDeferralSurvivesRestartAndReconcilesNewHead(t *testing.T) {
	ctx := t.Context()
	store := sqlitetest.MustOpen(t)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(30 * time.Minute)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID:     "p",
		Path:   t.TempDir(),
		Config: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex}}},
	}); err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		AutoReviewEnabled: true,
		Activity:          domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-5 * time.Minute)},
		CreatedAt:         now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteSCMObservation(ctx, domain.PullRequest{
		URL: "https://github.com/o/r/pull/1", Number: 1, SessionID: sess.ID, HeadSHA: "sha-1", UpdatedAt: now,
	}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}

	exhaustedSource := quotaTestSource{
		{Harness: domain.HarnessCodex}: {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(2 * time.Hour), ResetAt: resetAt, Exhausted: true}},
		{Harness: domain.HarnessAgy}:   {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(2 * time.Hour), ResetAt: now.Add(time.Hour), Exhausted: true}},
	}
	trig := &recordingOptionsTrigger{}
	coord1 := New(store, trig, Config{
		Clock: func() time.Time { return now },
		Quota: &workerquota.Policy{
			Role:      workerquota.RoleReviewer,
			Fallbacks: []workerquota.Target{{Harness: domain.HarnessAgy}},
			Source:    exhaustedSource,
		},
	})
	res, err := coord1.EvaluateSession(ctx, sess.ID)
	if err != nil || res.Triggered || res.Reason != "reviewer_quota_deferred" || !res.RetryAt.Equal(resetAt) {
		t.Fatalf("coord1 EvaluateSession = %+v, err=%v", res, err)
	}

	// Simulate daemon restart: brand-new Coordinator backed by the same SQLite store.
	// Even if Quotum is temporarily unavailable, the persisted deferral holds until resetAt.
	coord2 := New(store, trig, Config{
		Clock: func() time.Time { return now.Add(5 * time.Minute) },
		Quota: &workerquota.Policy{
			Role:      workerquota.RoleReviewer,
			Fallbacks: []workerquota.Target{{Harness: domain.HarnessAgy}},
		},
	})
	res, err = coord2.EvaluateSession(ctx, sess.ID)
	if err != nil || res.Triggered || res.Reason != "reviewer_quota_deferred" || !res.RetryAt.Equal(resetAt) {
		t.Fatalf("coord2 after restart should honor durable deferral: res=%+v err=%v", res, err)
	}

	// A new commit (HeadSHA = sha-2) bypasses the stale deferral for sha-1.
	if err := store.WriteSCMObservation(ctx, domain.PullRequest{
		URL: "https://github.com/o/r/pull/1", Number: 1, SessionID: sess.ID, HeadSHA: "sha-2", UpdatedAt: now.Add(5 * time.Minute),
	}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}
	res, err = coord2.EvaluateSession(ctx, sess.ID)
	if err != nil || !res.Triggered {
		t.Fatalf("new HeadSHA should reconcile and trigger: res=%+v err=%v", res, err)
	}
}

func TestReviewerQuotaRouteReadySkipsUnavailableChatToTUI(t *testing.T) {
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
	agyChat := workerquota.Target{Harness: domain.HarnessAgy, Model: "agy-chat", Mode: domain.SessionModeChat}
	agyTUI := workerquota.Target{Harness: domain.HarnessAgy, Model: "agy-tui", Mode: domain.SessionModeTUI}
	trig := &recordingOptionsTrigger{}
	coord := New(st, trig, Config{
		Clock: func() time.Time { return now },
		Quota: &workerquota.Policy{
			Role:      workerquota.RoleReviewer,
			Fallbacks: []workerquota.Target{agyChat, agyTUI},
			Source: quotaTestSource{
				{Harness: domain.HarnessCodex}: {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), ResetAt: now.Add(2 * time.Hour), Exhausted: true}},
			},
		},
		RouteReady: func(_ context.Context, target workerquota.Target) bool {
			return target.Mode != domain.SessionModeChat
		},
	})
	res, err := coord.EvaluateSession(context.Background(), "p-1")
	if err != nil || !res.Triggered || len(trig.calls) != 1 {
		t.Fatalf("EvaluateSession = %+v, err=%v, calls=%+v", res, err, trig.calls)
	}
	if trig.calls[0].InterfaceMode != domain.ReviewerInterfaceTUI || trig.calls[0].Config.Model != "agy-tui" {
		t.Fatalf("selected = %+v, want agy-tui/tui", trig.calls[0])
	}
}

func TestReviewerQuotaLiveChatQuotaExhaustedWithoutResetSwitchesToFallback(t *testing.T) {
	ctx := t.Context()
	store := sqlitetest.MustOpen(t)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID:     "p",
		Path:   t.TempDir(),
		Config: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex}}},
	}); err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		AutoReviewEnabled: true,
		Activity:          domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-5 * time.Minute)},
		CreatedAt:         now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteSCMObservation(ctx, domain.PullRequest{
		URL: "https://github.com/o/r/pull/1", Number: 1, SessionID: sess.ID, HeadSHA: "sha-1", UpdatedAt: now,
	}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}
	// Seed a real reviewer row + failed Chat conversation turn with failure_code='quota_exhausted'
	// and NO usage_limit_resets_at (matching the live yuworker smoke test).
	if err := store.UpsertReview(ctx, domain.Review{
		ID: "rev-1", SessionID: sess.ID, ProjectID: "p", Harness: domain.ReviewerCodex,
		InterfaceMode: domain.ReviewerInterfaceChat, ControllerGeneration: "gen-1",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	conv, err := store.CreateReviewConversation(ctx, "conv-rev-1", "rev-1", "p", sess.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	failedAt := now.Add(-2 * time.Minute)
	if err := store.AdoptReviewProviderTurn(ctx, conv.ID, sess.ID, "rev-1", "gen-1", "turn-1", "prov-turn-1", failedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.SettleTurnWithFailureCode(ctx, conv.ID, "prov-turn-1", domain.TurnStateFailed, "usage limit exceeded", domain.ConversationFailureCode("quota_exhausted"), time.Time{}, failedAt); err != nil {
		t.Fatal(err)
	}

	// No Quotum configured — relies solely on durable SQLite reviewer quota refusal evidence!
	trig := &recordingOptionsTrigger{}
	coord := New(store, trig, Config{
		Clock:         func() time.Time { return now },
		QuotaFailures: store.WorkerQuotaFailures,
		Quota: &workerquota.Policy{
			Role:      workerquota.RoleReviewer,
			Fallbacks: []workerquota.Target{{Harness: domain.HarnessAgy, Mode: domain.SessionModeTUI}},
		},
	})
	res, err := coord.EvaluateSession(ctx, sess.ID)
	if err != nil || !res.Triggered || len(trig.calls) != 1 {
		t.Fatalf("EvaluateSession = %+v, err=%v, calls=%+v", res, err, trig.calls)
	}
	if trig.calls[0].Harness != domain.ReviewerAgy || !trig.calls[0].DeduplicateAcrossHarnesses {
		t.Fatalf("expected failover to AGY with cross-harness dedup, got %+v", trig.calls[0])
	}
}

func TestReviewerQuotaRetryCappedPrimaryYieldsToEligibleFallbackOnSameHead(t *testing.T) {
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
		runs: []domain.ReviewRun{
			{PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha-1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunFailed, TriggerSource: domain.ReviewTriggerAuto},
			{PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha-1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunFailed, TriggerSource: domain.ReviewTriggerAuto},
			{PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha-1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunFailed, TriggerSource: domain.ReviewTriggerAuto},
		},
	}
	trig := &recordingOptionsTrigger{}
	coord := New(st, trig, Config{
		Clock: func() time.Time { return now },
		Quota: &workerquota.Policy{
			Role:      workerquota.RoleReviewer,
			Fallbacks: []workerquota.Target{{Harness: domain.HarnessAgy, Mode: domain.SessionModeTUI}},
			// Codex quota has recovered (non-exhausted), but Codex already hit its per-SHA retry cap.
			Source: quotaTestSource{
				{Harness: domain.HarnessCodex}: {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), Exhausted: false}},
				{Harness: domain.HarnessAgy}:   {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), Exhausted: false}},
			},
		},
	})
	res, err := coord.EvaluateSession(context.Background(), "p-1")
	if err != nil || !res.Triggered || len(trig.calls) != 1 {
		t.Fatalf("expected retry-capped Codex to yield to AGY on same HEAD, got res=%+v err=%v calls=%+v", res, err, trig.calls)
	}
	if trig.calls[0].Harness != domain.ReviewerAgy {
		t.Fatalf("selected harness = %q, want agy", trig.calls[0].Harness)
	}
}

func TestReviewerQuotaFreshPositiveEvidenceWakesPersistedDeferralOnSameHead(t *testing.T) {
	ctx := t.Context()
	store := sqlitetest.MustOpen(t)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(30 * time.Minute)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID:     "p",
		Path:   t.TempDir(),
		Config: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex}}},
	}); err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		AutoReviewEnabled: true,
		Activity:          domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-5 * time.Minute)},
		CreatedAt:         now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteSCMObservation(ctx, domain.PullRequest{
		URL: "https://github.com/o/r/pull/1", Number: 1, SessionID: sess.ID, HeadSHA: "sha-1", UpdatedAt: now,
	}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}

	// Persist deferral when all routes are exhausted.
	trig := &recordingOptionsTrigger{}
	coord1 := New(store, trig, Config{
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
	if res, err := coord1.EvaluateSession(ctx, sess.ID); err != nil || res.Triggered || res.Reason != "reviewer_quota_deferred" {
		t.Fatalf("coord1 = %+v, err=%v", res, err)
	}

	// Before resetAt, fresh positive evidence arrives for AGY on the SAME HEAD -> wakes immediately!
	coord2 := New(store, trig, Config{
		Clock: func() time.Time { return now.Add(5 * time.Minute) },
		Quota: &workerquota.Policy{
			Role:      workerquota.RoleReviewer,
			Fallbacks: []workerquota.Target{{Harness: domain.HarnessAgy, Mode: domain.SessionModeTUI}},
			Source: quotaTestSource{
				{Harness: domain.HarnessCodex}:                            {{ObservedAt: now.Add(4 * time.Minute), ValidUntil: now.Add(2 * time.Hour), ResetAt: resetAt, Exhausted: true}},
				{Harness: domain.HarnessAgy, Mode: domain.SessionModeTUI}: {{ObservedAt: now.Add(4 * time.Minute), ValidUntil: now.Add(2 * time.Hour), Exhausted: false}},
			},
		},
	})
	res, err := coord2.EvaluateSession(ctx, sess.ID)
	if err != nil || !res.Triggered || len(trig.calls) != 1 || trig.calls[0].Harness != domain.ReviewerAgy {
		t.Fatalf("fresh positive evidence should wake same HEAD: res=%+v err=%v calls=%+v", res, err, trig.calls)
	}
}

func TestReviewerQuotaAllProvidersQuotaFailedRecoverAfterResetOnSameHead(t *testing.T) {
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
		// Both Codex and AGY previously failed 3 times on sha-1 due to quota_exhausted.
		runs: []domain.ReviewRun{
			{PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha-1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunFailed, Body: "reviewer Chat turn failed: quota_exhausted", TriggerSource: domain.ReviewTriggerAuto},
			{PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha-1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunFailed, Body: "reviewer Chat turn failed: quota_exhausted", TriggerSource: domain.ReviewTriggerAuto},
			{PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha-1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunFailed, Body: "reviewer Chat turn failed: quota_exhausted", TriggerSource: domain.ReviewTriggerAuto},
			{PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha-1", Harness: domain.ReviewerAgy, Status: domain.ReviewRunFailed, Body: "reviewer Chat turn failed: quota_exhausted", TriggerSource: domain.ReviewTriggerAuto},
			{PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha-1", Harness: domain.ReviewerAgy, Status: domain.ReviewRunFailed, Body: "reviewer Chat turn failed: quota_exhausted", TriggerSource: domain.ReviewTriggerAuto},
			{PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha-1", Harness: domain.ReviewerAgy, Status: domain.ReviewRunFailed, Body: "reviewer Chat turn failed: quota_exhausted", TriggerSource: domain.ReviewTriggerAuto},
		},
	}
	trig := &recordingOptionsTrigger{}
	coord := New(st, trig, Config{
		Clock: func() time.Time { return now },
		Quota: &workerquota.Policy{
			Role:      workerquota.RoleReviewer,
			Fallbacks: []workerquota.Target{{Harness: domain.HarnessAgy, Mode: domain.SessionModeTUI}},
			Source: quotaTestSource{
				{Harness: domain.HarnessCodex}: {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), Exhausted: false}},
			},
		},
	})
	res, err := coord.EvaluateSession(context.Background(), "p-1")
	if err != nil || !res.Triggered || len(trig.calls) != 1 || trig.calls[0].Harness != domain.ReviewerCodex {
		t.Fatalf("quota-failed runs must not permanently strand same HEAD after reset: res=%+v err=%v calls=%+v", res, err, trig.calls)
	}
}

func TestReviewerQuotaActiveDeferralScansPastUnknownPrimaryToPositiveFallback(t *testing.T) {
	ctx := t.Context()
	store := sqlitetest.MustOpen(t)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(30 * time.Minute)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID:     "p",
		Path:   t.TempDir(),
		Config: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex}}},
	}); err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		AutoReviewEnabled: true,
		Activity:          domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-5 * time.Minute)},
		CreatedAt:         now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteSCMObservation(ctx, domain.PullRequest{
		URL: "https://github.com/o/r/pull/1", Number: 1, SessionID: sess.ID, HeadSHA: "sha-1", UpdatedAt: now,
	}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}

	trig := &recordingOptionsTrigger{}
	coord1 := New(store, trig, Config{
		Clock: func() time.Time { return now },
		Quota: &workerquota.Policy{
			Role:      workerquota.RoleReviewer,
			Fallbacks: []workerquota.Target{{Harness: domain.HarnessAgy, Mode: domain.SessionModeTUI}},
			Source: quotaTestSource{
				{Harness: domain.HarnessCodex}:                            {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(2 * time.Hour), ResetAt: resetAt, Exhausted: true}},
				{Harness: domain.HarnessAgy, Mode: domain.SessionModeTUI}: {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(2 * time.Hour), ResetAt: now.Add(time.Hour), Exhausted: true}},
			},
		},
	})
	if res, err := coord1.EvaluateSession(ctx, sess.ID); err != nil || res.Triggered || res.Reason != "reviewer_quota_deferred" {
		t.Fatalf("coord1 = %+v, err=%v", res, err)
	}

	// Primary Codex's window disappears (unknown quota), while fallback AGY reports fresh positive capacity.
	coord2 := New(store, trig, Config{
		Clock: func() time.Time { return now.Add(5 * time.Minute) },
		Quota: &workerquota.Policy{
			Role:      workerquota.RoleReviewer,
			Fallbacks: []workerquota.Target{{Harness: domain.HarnessAgy, Mode: domain.SessionModeTUI}},
			Source: quotaTestSource{
				{Harness: domain.HarnessAgy, Mode: domain.SessionModeTUI}: {{ObservedAt: now.Add(4 * time.Minute), ValidUntil: now.Add(2 * time.Hour), Exhausted: false}},
			},
		},
	})
	res, err := coord2.EvaluateSession(ctx, sess.ID)
	if err != nil || !res.Triggered || len(trig.calls) != 1 || trig.calls[0].Harness != domain.ReviewerAgy {
		t.Fatalf("active deferral should scan past unknown primary to positive fallback: res=%+v err=%v calls=%+v", res, err, trig.calls)
	}
}
