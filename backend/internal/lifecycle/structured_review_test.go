package lifecycle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/structuredreview"
)

func TestApplyPRObservation_StructuredReview_Issue21ExactOnceAndRestart(t *testing.T) {
	const (
		prURL = "https://github.com/wangyu0426/agent-orchestrator/pull/21"
		head1 = "1111111111111111111111111111111111111111"
	)
	store := newFakeStore()
	sid := domain.SessionID("worker-21")
	store.sessions[sid] = domain.SessionRecord{
		ID:       sid,
		Activity: domain.Activity{State: domain.ActivityIdle},
	}
	store.prs[sid] = []domain.PullRequest{{
		URL:     prURL,
		Number:  21,
		Author:  "wangyu0426",
		HeadSHA: head1,
	}}
	// Resolved inline thread + authorized same-account structured REQUEST_CHANGES
	// on a COMMENTED review, followed by informal prose COMMENTED review.
	store.comments[prURL] = []domain.PullRequestComment{
		{ID: "c1", ThreadID: "t1", Author: "wangyu0426", File: "a.go", Line: 10, Body: "resolved", Resolved: true, AutoInjectReview: true},
	}
	store.reviews[prURL] = []domain.PullRequestReview{
		{
			ID:               "PRR_6035617873",
			Author:           "wangyu0426",
			State:            domain.ReviewNone,
			URL:              prURL + "#pullrequestreview-6035617873",
			Body:             "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + head1 + "\n\n[P2] Finding 1\n[P2] Finding 2",
			TargetSHA:        head1,
			SubmittedAt:      time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
			AutoInjectReview: true,
		},
		{
			ID:               "PRR_informal",
			Author:           "wangyu0426",
			State:            domain.ReviewNone,
			URL:              prURL + "#pullrequestreview-999",
			Body:             "Looks great otherwise!",
			TargetSHA:        head1,
			SubmittedAt:      time.Date(2026, 11, 24, 12, 5, 0, 0, time.UTC),
			AutoInjectReview: true,
		},
	}

	msg := &fakeMessenger{}
	policy := structuredreview.Policy{Enabled: true, AllowPRAuthor: true}
	m := New(store, msg, WithStructuredReviewPolicy(policy))

	obs := ports.PRObservation{
		Fetched: true,
		URL:     prURL,
		Number:  21,
		Review:  domain.ReviewNone,
	}

	// Concurrent overlapping polls must deliver exactly once.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.ApplyPRObservation(context.Background(), sid, obs); err != nil {
				t.Errorf("ApplyPRObservation: %v", err)
			}
		}()
	}
	wg.Wait()

	if len(msg.msgs) != 1 {
		t.Fatalf("expected exactly 1 nudge, got %d", len(msg.msgs))
	}
	gotBody := msg.msgs[0]
	for _, want := range []string{head1, "[P2] Finding 1", "[P2] Finding 2", "PRR_6035617873", prURL} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("nudge missing %q:\n%s", want, gotBody)
		}
	}

	// Simulate daemon restart: brand new Manager over the same store must NOT
	// re-deliver the already-delivered structured review.
	restarted := New(store, msg, WithStructuredReviewPolicy(policy))
	if err := restarted.ApplyPRObservation(context.Background(), sid, obs); err != nil {
		t.Fatalf("restarted ApplyPRObservation: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("expected still 1 nudge after restart, got %d", len(msg.msgs))
	}

	// scmObservationReadyToMerge must return false while structured REQUEST_CHANGES is active.
	ready, err := restarted.scmObservationReadyToMerge(context.Background(), ports.SCMObservation{
		Fetched: true,
		PR:      ports.SCMPRObservation{URL: prURL, Author: "wangyu0426", HeadSHA: head1},
		CI:      ports.SCMCIObservation{Summary: string(domain.CIPassing)},
		Review:  ports.SCMReviewObservation{Decision: string(domain.ReviewNone)},
		Mergeability: ports.SCMMergeabilityObservation{
			State: string(domain.MergeMergeable),
		},
	})
	if err != nil {
		t.Fatalf("scmObservationReadyToMerge: %v", err)
	}
	if ready {
		t.Fatalf("expected PR with active structured REQUEST_CHANGES not to be ready to merge")
	}

	// PRReviewDiagnostics exposes delivered status while still unaddressed,
	// keeping GitHub's native decision separate from AO's local decision.
	diag, ok, err := restarted.PRReviewDiagnostics(context.Background(), prURL)
	if err != nil || !ok {
		t.Fatalf("PRReviewDiagnostics: ok=%v err=%v", ok, err)
	}
	if diag.GitHubDecision != domain.ReviewNone || diag.EffectiveDecision != domain.ReviewChangesRequest || diag.Source != structuredreview.SourceStructured {
		t.Fatalf("unexpected diagnostics: %+v", diag)
	}
	if len(diag.PendingFeedback) != 1 || diag.PendingFeedback[0].DeliveryStatus != structuredreview.DeliveryDelivered {
		t.Fatalf("expected 1 delivered pending feedback item, got %+v", diag.PendingFeedback)
	}
}

func TestApplyPRObservation_StructuredReview_DeliveryFailureRetries(t *testing.T) {
	const (
		prURL = "https://github.com/wangyu0426/agent-orchestrator/pull/25"
		head1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	store := newFakeStore()
	sid := domain.SessionID("worker-25")
	store.sessions[sid] = domain.SessionRecord{
		ID:       sid,
		Activity: domain.Activity{State: domain.ActivityIdle},
	}
	store.prs[sid] = []domain.PullRequest{{
		URL:     prURL,
		Number:  25,
		Author:  "wangyu0426",
		HeadSHA: head1,
	}}
	store.reviews[prURL] = []domain.PullRequestReview{
		{
			ID:               "PRR_retry",
			Author:           "wangyu0426",
			State:            domain.ReviewNone,
			URL:              prURL + "#pullrequestreview-1",
			Body:             "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + head1 + "\n\n[P2] Retry me",
			TargetSHA:        head1,
			SubmittedAt:      time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
			AutoInjectReview: true,
		},
	}

	now := time.Date(2026, 11, 24, 12, 5, 0, 0, time.UTC)
	msg := &fakeMessenger{err: errors.New("pane busy")}
	m := New(store, msg, WithStructuredReviewPolicy(structuredreview.Policy{Enabled: true, AllowPRAuthor: true}))
	m.clock = func() time.Time { return now }
	obs := ports.PRObservation{Fetched: true, URL: prURL, Number: 25, Review: domain.ReviewNone}

	// First attempt fails: must NOT mark delivered in Seen, but persists retry backoff state.
	if err := m.ApplyPRObservation(context.Background(), sid, obs); err == nil {
		t.Fatalf("expected error on first delivery attempt")
	}
	if strings.Contains(store.signatures[prURL], `"seen"`) {
		t.Fatalf("failed delivery must not mark seen in nudge signature, got %q", store.signatures[prURL])
	}

	// Clear error and advance past backoff before retry: must deliver once and persist Seen.
	msg.err = nil
	now = now.Add(feedbackRetryBaseDelay)
	if err := m.ApplyPRObservation(context.Background(), sid, obs); err != nil {
		t.Fatalf("retry ApplyPRObservation: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("expected 1 successful delivery after retry, got %d", len(msg.msgs))
	}
	if !strings.Contains(store.signatures[prURL], `"seen"`) {
		t.Fatalf("expected durable seen nudge signature after successful delivery, got %q", store.signatures[prURL])
	}
}

func TestApplyPRObservation_NativeAppChangesRequestedReachesWorkerOnce(t *testing.T) {
	const (
		prURL = "https://github.com/org/repo/pull/68"
		head  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	store := newFakeStore()
	sid := domain.SessionID("worker-68")
	store.sessions[sid] = domain.SessionRecord{ID: sid, Activity: domain.Activity{State: domain.ActivityIdle}}
	store.prs[sid] = []domain.PullRequest{{URL: prURL, Number: 68, Author: "worker-app[bot]", HeadSHA: head}}
	store.reviews[prURL] = []domain.PullRequestReview{{
		ID: "PRR_native_68", Author: "reviewer-app[bot]", IsBot: true,
		State: domain.ReviewChangesRequest, URL: prURL + "#pullrequestreview-68",
		Body: "Please fix the nil guard.", TargetSHA: head,
		SubmittedAt: time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC), AutoInjectReview: true,
	}}
	msg := &fakeMessenger{}
	m := New(store, msg)
	obs := ports.PRObservation{
		Fetched: true, URL: prURL, Number: 68, Review: domain.ReviewChangesRequest,
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.ApplyPRObservation(context.Background(), sid, obs); err != nil {
				t.Errorf("ApplyPRObservation: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(msg.msgs) != 1 {
		t.Fatalf("native bot review should reach worker once; got %d messages: %v", len(msg.msgs), msg.msgs)
	}
	for _, want := range []string{"reviewer-app[bot]", "Please fix the nil guard.", "PRR_native_68", prURL} {
		if !strings.Contains(msg.msgs[0], want) {
			t.Fatalf("worker feedback missing %q: %s", want, msg.msgs[0])
		}
	}
	if err := m.ApplyPRObservation(context.Background(), sid, obs); err != nil {
		t.Fatalf("repeat observation: %v", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("repeated native bot review delivered %d times, want once", len(msg.msgs))
	}
}
