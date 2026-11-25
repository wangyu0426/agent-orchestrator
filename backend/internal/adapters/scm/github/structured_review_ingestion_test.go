package github

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/structuredreview"
)

func TestGitHubReviewIngestionIntoStructuredReview(t *testing.T) {
	const head1 = "1111111111111111111111111111111111111111"

	// Verify GraphQL query only requests submitted states (excluding PENDING and DISMISSED).
	q := buildReviewThreadsQuery(ports.SCMPRRef{
		Repo:   ports.SCMRepo{Owner: "wangyu0426", Name: "agent-orchestrator"},
		Number: 21,
	}, "", true)
	if !strings.Contains(q, "states:[APPROVED,CHANGES_REQUESTED,COMMENTED]") {
		t.Fatalf("expected review query to restrict to submitted states, got:\n%s", q)
	}
	if strings.Contains(q, "PENDING") || strings.Contains(q, "DISMISSED") {
		t.Fatalf("review query must not include PENDING or DISMISSED states:\n%s", q)
	}

	// Simulate GraphQL payload for a submitted COMMENTED review with structured header.
	raw := map[string]any{
		"id":          "PRR_kwDO123",
		"state":       "COMMENTED",
		"url":         "https://github.com/wangyu0426/agent-orchestrator/pull/21#pullrequestreview-6035617873",
		"submittedAt": "2026-11-24T12:00:00Z",
		"body":        "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + head1 + "\n\n[P2] Fix quota routing.",
		"commit":      map[string]any{"oid": head1},
		"author":      map[string]any{"login": "wangyu0426", "__typename": "User"},
	}
	summary := scmReviewSummaryFromGraphQL(raw)
	review := domain.PullRequestReview{
		ID:          summary.ID,
		Author:      summary.Author,
		State:       domain.ReviewDecision(summary.State),
		URL:         summary.URL,
		Body:        summary.Body,
		IsBot:       summary.IsBot,
		TargetSHA:   summary.TargetSHA,
		SubmittedAt: summary.SubmittedAt,
	}

	eval := structuredreview.Evaluate(structuredreview.EvalInput{
		Policy:         structuredreview.Policy{Enabled: true, AllowPRAuthor: true},
		PRAuthor:       "wangyu0426",
		CurrentHeadSHA: head1,
		GitHubDecision: domain.ReviewNone,
		Reviews:        []domain.PullRequestReview{review},
	})
	if eval.EffectiveDecision != domain.ReviewChangesRequest || eval.Source != structuredreview.SourceStructured {
		t.Fatalf("expected structured changes_requested from ingested GitHub review, got %+v", eval)
	}
	if len(eval.ActiveChangeRequests) != 1 || eval.ActiveChangeRequests[0].ReviewedSHA != head1 {
		t.Fatalf("unexpected active change requests: %+v", eval.ActiveChangeRequests)
	}
}
