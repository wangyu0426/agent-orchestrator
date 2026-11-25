package session

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/structuredreview"
)

type fakePRReviewDiagnosticsReader struct {
	byURL map[string]structuredreview.Diagnostics
}

func (f fakePRReviewDiagnosticsReader) PRReviewDiagnostics(_ context.Context, prURL string) (structuredreview.Diagnostics, bool, error) {
	d, ok := f.byURL[prURL]
	return d, ok, nil
}

func TestListPRSummaries_ExposesStructuredReviewDiagnostics(t *testing.T) {
	const (
		prURL = "https://github.com/wangyu0426/agent-orchestrator/pull/25"
		head1 = "1111111111111111111111111111111111111111"
		head2 = "2222222222222222222222222222222222222222"
	)
	store := newFakeStore()
	sid := domain.SessionID("worker-25")
	store.sessions[sid] = domain.SessionRecord{ID: sid, ProjectID: "proj-1"}
	store.prs[sid] = []domain.PullRequest{{
		URL:       prURL,
		SessionID: sid,
		Number:    25,
		Author:    "wangyu0426",
		HeadSHA:   head1,
		Review:    domain.ReviewNone,
	}}

	reader := fakePRReviewDiagnosticsReader{byURL: map[string]structuredreview.Diagnostics{
		prURL: {
			GitHubDecision:    domain.ReviewNone,
			EffectiveDecision: domain.ReviewChangesRequest,
			Source:            structuredreview.SourceStructured,
			ReviewedSHA:       head1,
			PendingFeedback: []structuredreview.PendingFeedbackDiagnostic{{
				ReviewID:       "PRR_1",
				Reviewer:       "wangyu0426",
				ReviewURL:      prURL + "#pullrequestreview-1",
				ReviewedSHA:    head1,
				Feedback:       "[P2] Fix it",
				DeliveryStatus: structuredreview.DeliveryDelivered,
			}},
		},
	}}
	svc := NewWithDeps(Deps{Store: store, PRReviewDiagnostics: reader})

	summaries, err := svc.ListPRSummaries(context.Background(), sid)
	if err != nil {
		t.Fatalf("ListPRSummaries: %v", err)
	}
	if len(summaries) != 1 || summaries[0].Review.Structured == nil {
		t.Fatalf("expected structured diagnostics on PR summary, got %+v", summaries)
	}
	got := summaries[0].Review
	if got.Decision != domain.ReviewNone {
		t.Fatalf("GitHub decision must remain separate: got %q, want none", got.Decision)
	}
	if got.Structured.GitHubDecision != domain.ReviewNone || got.Structured.EffectiveDecision != domain.ReviewChangesRequest {
		t.Fatalf("unexpected structured decisions: %+v", got.Structured)
	}
	if got.Structured.Source != string(structuredreview.SourceStructured) || got.Structured.ReviewedSHA != head1 {
		t.Fatalf("unexpected source/sha: %+v", got.Structured)
	}
	if len(got.Structured.PendingFeedback) != 1 || got.Structured.PendingFeedback[0].DeliveryStatus != string(structuredreview.DeliveryDelivered) {
		t.Fatalf("unexpected pending feedback: %+v", got.Structured.PendingFeedback)
	}

	// Valid HEAD-bound structured APPROVE is exposed with GitHub decision preserved.
	reader.byURL[prURL] = structuredreview.Diagnostics{
		GitHubDecision:    domain.ReviewNone,
		EffectiveDecision: domain.ReviewApproved,
		Source:            structuredreview.SourceStructured,
		ReviewedSHA:       head2,
	}
	summaries, err = svc.ListPRSummaries(context.Background(), sid)
	if err != nil {
		t.Fatalf("ListPRSummaries (approved): %v", err)
	}
	got = summaries[0].Review
	if got.Decision != domain.ReviewNone || got.Structured == nil || got.Structured.EffectiveDecision != domain.ReviewApproved || got.Structured.ReviewedSHA != head2 {
		t.Fatalf("expected HEAD-bound structured approval with native decision preserved, got %+v", got)
	}
}
