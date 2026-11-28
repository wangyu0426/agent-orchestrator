package scm

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type memConversationFeedbackStore struct {
	initialized map[string]time.Time
	rows        map[string]map[string]domain.PullRequestConversationFeedback
}

func newMemConversationFeedbackStore() *memConversationFeedbackStore {
	return &memConversationFeedbackStore{
		initialized: map[string]time.Time{},
		rows:        map[string]map[string]domain.PullRequestConversationFeedback{},
	}
}

func (m *memConversationFeedbackStore) GetPRConversationFeedbackState(_ context.Context, prURL string) (time.Time, bool, error) {
	t, ok := m.initialized[prURL]
	return t, ok, nil
}

func (m *memConversationFeedbackStore) MarkPRConversationFeedbackInitialized(_ context.Context, prURL string, at time.Time) error {
	if _, ok := m.initialized[prURL]; !ok {
		m.initialized[prURL] = at
	}
	return nil
}

func (m *memConversationFeedbackStore) ListPRConversationFeedback(_ context.Context, prURL string) ([]domain.PullRequestConversationFeedback, error) {
	byID := m.rows[prURL]
	out := make([]domain.PullRequestConversationFeedback, 0, len(byID))
	for _, r := range byID {
		out = append(out, r)
	}
	return out, nil
}

func (m *memConversationFeedbackStore) UpsertPRConversationFeedback(_ context.Context, item domain.PullRequestConversationFeedback) error {
	if m.rows[item.PRURL] == nil {
		m.rows[item.PRURL] = map[string]domain.PullRequestConversationFeedback{}
	}
	m.rows[item.PRURL][item.CommentID] = item
	return nil
}

func TestReconcileConversationFeedback_PartialWindowExcludesDeletedCommand(t *testing.T) {
	obs := New(nil, nil, nil, Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	fbStore := newMemConversationFeedbackStore()
	prURL := "https://github.com/wangyu0426/WebPoB/pull/9"
	now := time.Date(2026, 11, 28, 5, 0, 0, 0, time.UTC)

	subj := &subject{
		session: domain.SessionRecord{
			ID:               "webpob-1",
			AutoInjectReview: true,
			Activity:         domain.Activity{State: domain.ActivityWaitingInput, LastActivityAt: now},
		},
		known: domain.PullRequest{URL: prURL, Number: 9},
		hasPR: true,
	}
	cfg := domain.PRCommentFeedbackConfig{
		Enabled:        true,
		TrustedAuthors: []string{"wangyu0426"},
	}

	// 1. Busy worker: authorized command is observed and persisted as active.
	hasActive, err := obs.reconcileConversationFeedback(context.Background(), fbStore, subj, cfg, ports.SCMConversationCommentsResult{
		Comments: []ports.SCMConversationCommentObservation{{
			ID:        "IC_old_cmd",
			Author:    "wangyu0426",
			Body:      "/ao review-feedback Fix edge case",
			CreatedAt: now.Add(-10 * time.Minute),
		}},
		Partial: false,
	}, now)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !hasActive {
		t.Fatal("expected hasActive=true after observing authorized command")
	}

	// 2. Partial window (newest 1,000 comments) excludes the deleted command with no newer command.
	subj.session.Activity.State = domain.ActivityIdle
	hasActive, err = obs.reconcileConversationFeedback(context.Background(), fbStore, subj, cfg, ports.SCMConversationCommentsResult{
		Comments: []ports.SCMConversationCommentObservation{{
			ID:        "IC_newer_unmarked",
			Author:    "wangyu0426",
			Body:      "Unmarked chatter",
			CreatedAt: now.Add(-1 * time.Minute),
		}},
		Partial: true,
	}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("partial reconcile: %v", err)
	}
	if hasActive {
		t.Fatal("expected hasActive=false when partial window excludes deleted command")
	}
	rows, err := fbStore.ListPRConversationFeedback(context.Background(), prURL)
	if err != nil {
		t.Fatalf("ListPRConversationFeedback: %v", err)
	}
	if len(rows) != 1 || rows[0].State != domain.PRConversationFeedbackActive {
		t.Fatalf("expected IC_old_cmd to be preserved as active on partial scan (not falsely deleted), got %+v", rows)
	}
}
