package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/structuredreview"
)

func TestReconcileResolvedNotifications_StructuredReviewParity(t *testing.T) {
	ctx := context.Background()
	s, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer s.Close()

	s.SetStructuredReviewPolicy(structuredreview.Policy{
		Enabled:       true,
		AllowPRAuthor: true,
	})

	const (
		prURL = "https://github.com/wangyu0426/agent-orchestrator/pull/25"
		head1 = "1111111111111111111111111111111111111111"
	)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	if err := s.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-1"}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	created, err := s.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "proj-1",
		Kind:      domain.KindWorker,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.ClaimPR(ctx, domain.PullRequest{
		URL:          prURL,
		SessionID:    created.ID,
		Number:       25,
		Author:       "wangyu0426",
		HeadSHA:      head1,
		CI:           domain.CIPassing,
		Review:       domain.ReviewNone,
		Mergeability: domain.MergeMergeable,
		UpdatedAt:    now,
	}, nil, []domain.PullRequestReview{{
		ID:          "PRR_1",
		Author:      "wangyu0426",
		State:       domain.ReviewNone,
		URL:         prURL + "#pullrequestreview-1",
		Body:        "AO-REVIEW: REQUEST_CHANGES\nHEAD: " + head1 + "\n\n[P2] Fix it",
		TargetSHA:   head1,
		SubmittedAt: now,
	}}, nil, nil, ports.ReviewWriteReplace, false); err != nil {
		t.Fatalf("ClaimPR: %v", err)
	}

	if _, _, err := s.CreateNotification(ctx, domain.NotificationRecord{
		ID:        "notif-ready-25",
		SessionID: created.ID,
		PRURL:     prURL,
		Type:      domain.NotificationReadyToMerge,
		Status:    domain.NotificationUnread,
		Title:     "Ready to merge",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateNotification: %v", err)
	}

	resolved, err := s.ReconcileResolvedNotifications(ctx, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ReconcileResolvedNotifications: %v", err)
	}
	if len(resolved) != 1 || resolved[0].ID != "notif-ready-25" {
		t.Fatalf("expected stale ready_to_merge notification to be resolved under active structured REQUEST_CHANGES, got %+v", resolved)
	}
}
