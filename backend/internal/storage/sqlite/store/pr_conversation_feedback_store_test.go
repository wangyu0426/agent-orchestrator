package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestPRConversationFeedback_UnverifiedRowsAreNotListedForDelivery(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	prURL := "https://github.com/example/repo/pull/9"
	if err := s.WritePR(ctx, domain.PullRequest{URL: prURL, SessionID: rec.ID, Number: 9}, nil, nil); err != nil {
		t.Fatalf("write PR: %v", err)
	}
	item := domain.PullRequestConversationFeedback{
		PRURL:             prURL,
		CommentID:         "IC_old_cmd",
		SessionID:         rec.ID,
		Author:            "operator",
		Body:              "Fix edge case",
		BodyHash:          "body-hash",
		State:             domain.PRConversationFeedbackActive,
		AutoInjectReview:  true,
		CreatedAtProvider: time.Now().UTC(),
		ObservedAt:        time.Now().UTC(),
	}
	if err := s.UpsertPRConversationFeedback(ctx, item); err != nil {
		t.Fatalf("upsert verified feedback: %v", err)
	}
	active, err := s.ListActivePRConversationFeedback(ctx, prURL)
	if err != nil || len(active) != 1 || active[0].Unverified {
		t.Fatalf("verified active rows = %+v, err=%v; want one verified row", active, err)
	}

	item.Unverified = true
	if err := s.UpsertPRConversationFeedback(ctx, item); err != nil {
		t.Fatalf("upsert unverified feedback: %v", err)
	}
	active, err = s.ListActivePRConversationFeedback(ctx, prURL)
	if err != nil || len(active) != 0 {
		t.Fatalf("delivery rows = %+v, err=%v; want no unverified rows", active, err)
	}
	all, err := s.ListPRConversationFeedback(ctx, prURL)
	if err != nil || len(all) != 1 || !all[0].Unverified {
		t.Fatalf("persisted rows = %+v, err=%v; want unverified state preserved", all, err)
	}

	item.Unverified = false
	if err := s.UpsertPRConversationFeedback(ctx, item); err != nil {
		t.Fatalf("reverify feedback: %v", err)
	}
	active, err = s.ListActivePRConversationFeedback(ctx, prURL)
	if err != nil || len(active) != 1 || active[0].Unverified {
		t.Fatalf("delivery rows after reverify = %+v, err=%v; want one verified row", active, err)
	}
}
