package structuredreview

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const (
	sha1 = "1111111111111111111111111111111111111111"
	sha2 = "2222222222222222222222222222222222222222"
)

func TestParseBody(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantOK       bool
		wantDecision domain.ReviewDecision
		wantSHA      string
		wantFeedback string
	}{
		{
			name:         "valid REQUEST_CHANGES",
			body:         "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + sha1 + "\n\n[P2] Fix quota routing fallback.",
			wantOK:       true,
			wantDecision: domain.ReviewChangesRequest,
			wantSHA:      sha1,
			wantFeedback: "[P2] Fix quota routing fallback.",
		},
		{
			name:         "valid REQUEST_CHANGES with CRLF and uppercase hex",
			body:         "AO-REVIEW: REQUEST_CHANGES\r\nHEAD: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\r\n\r\nFix nil guard.",
			wantOK:       true,
			wantDecision: domain.ReviewChangesRequest,
			wantSHA:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			wantFeedback: "Fix nil guard.",
		},
		{
			name:         "valid APPROVE without body",
			body:         "AO-REVIEW: APPROVE\nHEAD: " + sha1,
			wantOK:       true,
			wantDecision: domain.ReviewApproved,
			wantSHA:      sha1,
			wantFeedback: "",
		},
		{
			name:   "empty REQUEST_CHANGES rejected",
			body:   "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + sha1 + "\n\n   \n",
			wantOK: false,
		},
		{
			name:   "quoted marker rejected",
			body:   "> AO-REVIEW: REQUEST_CHANGES\n> HEAD: " + sha1 + "\n\n[P2] example",
			wantOK: false,
		},
		{
			name:   "prose before header rejected",
			body:   "Looks good overall, example:\nAO-REVIEW: APPROVE\nHEAD: " + sha1,
			wantOK: false,
		},
		{
			name:   "short SHA rejected",
			body:   "AO-REVIEW: APPROVE\nHEAD: 1111111",
			wantOK: false,
		},
		{
			name:   "non-hex SHA rejected",
			body:   "AO-REVIEW: APPROVE\nHEAD: gggggggggggggggggggggggggggggggggggggggg",
			wantOK: false,
		},
		{
			name:   "missing HEAD line rejected",
			body:   "AO-REVIEW: REQUEST_CHANGES\n\n[P2] missing head",
			wantOK: false,
		},
		{
			name:   "unknown decision rejected",
			body:   "AO-REVIEW: COMMENT\nHEAD: " + sha1 + "\n\nhello",
			wantOK: false,
		},
		{
			name:   "ordinary prose rejected",
			body:   "LGTM! Nice work on the tests.",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseBody(tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ParseBody() ok = %v, want %v (got=%+v)", ok, tc.wantOK, got)
			}
			if !ok {
				return
			}
			if got.Decision != tc.wantDecision || got.HeadSHA != tc.wantSHA || got.Feedback != tc.wantFeedback {
				t.Fatalf("ParseBody() = %+v, want decision=%q sha=%q feedback=%q", got, tc.wantDecision, tc.wantSHA, tc.wantFeedback)
			}
		})
	}
}

func TestEvaluate_Issue21ScenarioAndSameAccount(t *testing.T) {
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	policy := Policy{
		Enabled:       true,
		AllowPRAuthor: true,
	}

	// Same-account structured REQUEST_CHANGES on COMMENTED review, followed by
	// informal prose COMMENTED review: informal prose must NOT clear it.
	eval := Evaluate(EvalInput{
		Policy:               policy,
		PRAuthor:             "wangyu0426",
		CurrentHeadSHA:       sha1,
		GitHubDecision:       domain.ReviewNone,
		HasUnresolvedThreads: false,
		Reviews: []domain.PullRequestReview{
			{
				ID:          "PRR_1",
				Author:      "wangyu0426",
				State:       domain.ReviewNone,
				URL:         "https://github.com/wangyu0426/agent-orchestrator/pull/21#pullrequestreview-1",
				TargetSHA:   sha1,
				SubmittedAt: t0,
				Body:        "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + sha1 + "\n\n[P2] Finding 1\n[P2] Finding 2",
			},
			{
				ID:          "PRR_2",
				Author:      "wangyu0426",
				State:       domain.ReviewNone,
				TargetSHA:   sha1,
				SubmittedAt: t0.Add(time.Minute),
				Body:        "Thanks, looks much better now!",
			},
		},
	})

	if eval.GitHubDecision != domain.ReviewNone {
		t.Fatalf("GitHubDecision = %q, want none", eval.GitHubDecision)
	}
	if eval.EffectiveDecision != domain.ReviewChangesRequest {
		t.Fatalf("EffectiveDecision = %q, want changes_requested", eval.EffectiveDecision)
	}
	if eval.Source != SourceStructured || eval.ReviewedSHA != sha1 {
		t.Fatalf("Source=%q ReviewedSHA=%q, want structured/%s", eval.Source, eval.ReviewedSHA, sha1)
	}
	if len(eval.ActiveChangeRequests) != 1 || eval.ActiveChangeRequests[0].Review.ID != "PRR_1" {
		t.Fatalf("ActiveChangeRequests = %+v, want [PRR_1]", eval.ActiveChangeRequests)
	}

	// New commit alone does NOT clear structured REQUEST_CHANGES.
	evalNewHead := Evaluate(EvalInput{
		Policy:         policy,
		PRAuthor:       "wangyu0426",
		CurrentHeadSHA: sha2,
		GitHubDecision: domain.ReviewNone,
		Reviews: []domain.PullRequestReview{
			{
				ID:          "PRR_1",
				Author:      "wangyu0426",
				State:       domain.ReviewNone,
				TargetSHA:   sha1,
				SubmittedAt: t0,
				Body:        "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + sha1 + "\n\n[P2] Finding 1",
			},
		},
	})
	if evalNewHead.EffectiveDecision != domain.ReviewChangesRequest {
		t.Fatalf("new commit alone cleared REQUEST_CHANGES: got %q", evalNewHead.EffectiveDecision)
	}

	// Stale/ineligible APPROVE targeted at sha1 while CurrentHeadSHA is sha2
	// must NOT silently clear the active REQUEST_CHANGES on sha1.
	evalStaleApproveAfterCR := Evaluate(EvalInput{
		Policy:         policy,
		PRAuthor:       "wangyu0426",
		CurrentHeadSHA: sha2,
		GitHubDecision: domain.ReviewNone,
		Reviews: []domain.PullRequestReview{
			{
				ID:          "PRR_1",
				Author:      "wangyu0426",
				State:       domain.ReviewNone,
				TargetSHA:   sha1,
				SubmittedAt: t0,
				Body:        "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + sha1 + "\n\n[P2] Finding 1",
			},
			{
				ID:          "PRR_stale_ok",
				Author:      "wangyu0426",
				State:       domain.ReviewNone,
				TargetSHA:   sha1,
				SubmittedAt: t0.Add(time.Minute),
				Body:        "AO-REVIEW: APPROVE\nHEAD: " + sha1,
			},
		},
	})
	if evalStaleApproveAfterCR.EffectiveDecision != domain.ReviewChangesRequest {
		t.Fatalf("stale APPROVE on sha1 cleared REQUEST_CHANGES when HEAD is sha2: got %q", evalStaleApproveAfterCR.EffectiveDecision)
	}
	if len(evalStaleApproveAfterCR.ActiveChangeRequests) != 1 || evalStaleApproveAfterCR.ActiveChangeRequests[0].Review.ID != "PRR_1" {
		t.Fatalf("expected PRR_1 to remain active after stale APPROVE, got %+v", evalStaleApproveAfterCR.ActiveChangeRequests)
	}

	// Explicit structured APPROVE on sha2 supersedes PRR_1.
	evalApproved := Evaluate(EvalInput{
		Policy:         policy,
		PRAuthor:       "wangyu0426",
		CurrentHeadSHA: sha2,
		GitHubDecision: domain.ReviewNone,
		Reviews: []domain.PullRequestReview{
			{
				ID:          "PRR_1",
				Author:      "wangyu0426",
				State:       domain.ReviewNone,
				TargetSHA:   sha1,
				SubmittedAt: t0,
				Body:        "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + sha1 + "\n\n[P2] Finding 1",
			},
			{
				ID:          "PRR_3",
				Author:      "wangyu0426",
				State:       domain.ReviewNone,
				TargetSHA:   sha2,
				SubmittedAt: t0.Add(2 * time.Minute),
				Body:        "AO-REVIEW: APPROVE\nHEAD: " + sha2,
			},
		},
	})
	if evalApproved.EffectiveDecision != domain.ReviewApproved || evalApproved.Source != SourceStructured || evalApproved.ReviewedSHA != sha2 {
		t.Fatalf("expected structured approval on sha2, got %+v", evalApproved)
	}
	if len(evalApproved.ActiveChangeRequests) != 0 {
		t.Fatalf("expected 0 active change requests after approval, got %d", len(evalApproved.ActiveChangeRequests))
	}
}

func TestEvaluate_RejectionAndBoundaryRules(t *testing.T) {
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	policy := Policy{
		Enabled:          true,
		TrustedReviewers: []string{"alice"},
	}

	// Unauthorized reviewer, mismatched SHA, missing ID (pending), empty body.
	eval := Evaluate(EvalInput{
		Policy:         policy,
		PRAuthor:       "bob",
		CurrentHeadSHA: sha1,
		GitHubDecision: domain.ReviewNone,
		Reviews: []domain.PullRequestReview{
			{ID: "PRR_unauth", Author: "eve", State: domain.ReviewNone, TargetSHA: sha1, SubmittedAt: t0, Body: "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + sha1 + "\n\nfix"},
			{ID: "PRR_mismatch", Author: "alice", State: domain.ReviewNone, TargetSHA: sha2, SubmittedAt: t0, Body: "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + sha1 + "\n\nfix"},
			{ID: "", Author: "alice", State: domain.ReviewNone, TargetSHA: sha1, SubmittedAt: t0, Body: "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + sha1 + "\n\nfix"},
			{ID: "PRR_empty", Author: "alice", State: domain.ReviewNone, TargetSHA: sha1, SubmittedAt: t0, Body: "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + sha1 + "\n\n"},
		},
	})
	if eval.EffectiveDecision != domain.ReviewNone || len(eval.ActiveChangeRequests) != 0 {
		t.Fatalf("invalid reviews should not create decisions: %+v", eval)
	}

	// HEAD-bound approval: old approval on sha1 is invalidated when HEAD moves to sha2.
	evalStale := Evaluate(EvalInput{
		Policy:         policy,
		PRAuthor:       "bob",
		CurrentHeadSHA: sha2,
		GitHubDecision: domain.ReviewNone,
		Reviews: []domain.PullRequestReview{
			{ID: "PRR_old_ok", Author: "alice", State: domain.ReviewNone, TargetSHA: sha1, SubmittedAt: t0, Body: "AO-REVIEW: APPROVE\nHEAD: " + sha1},
		},
	})
	if evalStale.EffectiveDecision != domain.ReviewNone {
		t.Fatalf("stale approval must not authorize new HEAD: got %q", evalStale.EffectiveDecision)
	}

	// Structured APPROVE cannot override unresolved inline threads.
	evalThreads := Evaluate(EvalInput{
		Policy:               policy,
		PRAuthor:             "bob",
		CurrentHeadSHA:       sha1,
		GitHubDecision:       domain.ReviewNone,
		HasUnresolvedThreads: true,
		Reviews: []domain.PullRequestReview{
			{ID: "PRR_ok", Author: "alice", State: domain.ReviewNone, TargetSHA: sha1, SubmittedAt: t0, Body: "AO-REVIEW: APPROVE\nHEAD: " + sha1},
		},
	})
	if evalThreads.EffectiveDecision != domain.ReviewNone {
		t.Fatalf("structured approval must not override unresolved threads: got %q", evalThreads.EffectiveDecision)
	}

	// Structured APPROVE cannot override another reviewer's native CHANGES_REQUESTED
	// or GitHub REVIEW_REQUIRED.
	evalConflict := Evaluate(EvalInput{
		Policy:         policy,
		PRAuthor:       "bob",
		CurrentHeadSHA: sha1,
		GitHubDecision: domain.ReviewChangesRequest,
		Reviews: []domain.PullRequestReview{
			{ID: "PRR_native_cr", Author: "carol", State: domain.ReviewChangesRequest, TargetSHA: sha1, SubmittedAt: t0},
			{ID: "PRR_ok", Author: "alice", State: domain.ReviewNone, TargetSHA: sha1, SubmittedAt: t0.Add(time.Minute), Body: "AO-REVIEW: APPROVE\nHEAD: " + sha1},
		},
	})
	if evalConflict.EffectiveDecision != domain.ReviewChangesRequest || evalConflict.Source != SourceNative {
		t.Fatalf("conflicting reviewer native CR must win: %+v", evalConflict)
	}

	evalRequired := Evaluate(EvalInput{
		Policy:         policy,
		PRAuthor:       "bob",
		CurrentHeadSHA: sha1,
		GitHubDecision: domain.ReviewRequired,
		Reviews: []domain.PullRequestReview{
			{ID: "PRR_ok", Author: "alice", State: domain.ReviewNone, TargetSHA: sha1, SubmittedAt: t0, Body: "AO-REVIEW: APPROVE\nHEAD: " + sha1},
		},
	})
	if evalRequired.EffectiveDecision != domain.ReviewRequired {
		t.Fatalf("structured approval must not bypass REVIEW_REQUIRED: %+v", evalRequired)
	}
}

func TestBuildDiagnostics_PendingVsDelivered(t *testing.T) {
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	prURL := "https://github.com/wangyu0426/agent-orchestrator/pull/21"
	eval := Evaluate(EvalInput{
		Policy:         Policy{Enabled: true, AllowPRAuthor: true},
		PRAuthor:       "wangyu0426",
		CurrentHeadSHA: sha1,
		GitHubDecision: domain.ReviewNone,
		Reviews: []domain.PullRequestReview{
			{
				ID:          "PRR_1",
				Author:      "wangyu0426",
				State:       domain.ReviewNone,
				URL:         prURL + "#pullrequestreview-1",
				TargetSHA:   sha1,
				SubmittedAt: t0,
				Body:        "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + sha1 + "\n\n[P2] fix it",
			},
		},
	})

	diagPending := BuildDiagnostics(eval, prURL, "")
	if diagPending.GitHubDecision != domain.ReviewNone || diagPending.EffectiveDecision != domain.ReviewChangesRequest {
		t.Fatalf("unexpected decisions: %+v", diagPending)
	}
	if len(diagPending.PendingFeedback) != 1 || diagPending.PendingFeedback[0].DeliveryStatus != DeliveryPending {
		t.Fatalf("expected pending delivery status, got %+v", diagPending.PendingFeedback)
	}

	payloadBytes, _ := json.Marshal(map[string]any{
		"seen": map[string]string{
			NudgeKey(prURL, "PRR_1"): eval.ActiveChangeRequests[0].Signature,
		},
	})
	diagDelivered := BuildDiagnostics(eval, prURL, string(payloadBytes))
	if len(diagDelivered.PendingFeedback) != 1 || diagDelivered.PendingFeedback[0].DeliveryStatus != DeliveryDelivered {
		t.Fatalf("expected delivered status while still unaddressed, got %+v", diagDelivered.PendingFeedback)
	}
}
