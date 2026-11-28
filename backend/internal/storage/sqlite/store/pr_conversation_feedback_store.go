package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// GetPRConversationFeedbackState returns the baseline initialization timestamp
// for a PR's conversation feedback tracking, or ok=false if not yet initialized.
func (s *Store) GetPRConversationFeedbackState(ctx context.Context, prURL string) (time.Time, bool, error) {
	row, err := s.qr.GetPRConversationFeedbackState(ctx, prURL)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("get pr conversation feedback state %s: %w", prURL, err)
	}
	return row.InitializedAt.UTC(), true, nil
}

// MarkPRConversationFeedbackInitialized records that the PR's conversation
// feedback baseline has been established. Idempotent (ON CONFLICT DO NOTHING).
func (s *Store) MarkPRConversationFeedbackInitialized(ctx context.Context, prURL string, at time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.qw.UpsertPRConversationFeedbackState(ctx, gen.UpsertPRConversationFeedbackStateParams{
		PRURL:         prURL,
		InitializedAt: at.UTC(),
	}); err != nil {
		return fmt.Errorf("mark pr conversation feedback initialized %s: %w", prURL, err)
	}
	return nil
}

// ListPRConversationFeedback returns all persisted conversation feedback rows
// for prURL (active, baseline, deleted, and superseded).
func (s *Store) ListPRConversationFeedback(ctx context.Context, prURL string) ([]domain.PullRequestConversationFeedback, error) {
	rows, err := s.qr.ListPRConversationFeedback(ctx, prURL)
	if err != nil {
		return nil, fmt.Errorf("list pr conversation feedback %s: %w", prURL, err)
	}
	out := make([]domain.PullRequestConversationFeedback, 0, len(rows))
	for _, r := range rows {
		out = append(out, domainConversationFeedback(r))
	}
	return out, nil
}

// ListActivePRConversationFeedback returns only active conversation feedback
// rows for prURL.
func (s *Store) ListActivePRConversationFeedback(ctx context.Context, prURL string) ([]domain.PullRequestConversationFeedback, error) {
	rows, err := s.qr.ListActivePRConversationFeedback(ctx, prURL)
	if err != nil {
		return nil, fmt.Errorf("list active pr conversation feedback %s: %w", prURL, err)
	}
	out := make([]domain.PullRequestConversationFeedback, 0, len(rows))
	for _, r := range rows {
		out = append(out, domainConversationFeedback(r))
	}
	return out, nil
}

// UpsertPRConversationFeedback inserts or updates one conversation feedback row.
func (s *Store) UpsertPRConversationFeedback(ctx context.Context, item domain.PullRequestConversationFeedback) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	sourceType := item.SourceType
	if sourceType == "" {
		sourceType = domain.PRCommentFeedbackSourceType
	}
	state := string(item.State)
	if state == "" {
		state = string(domain.PRConversationFeedbackActive)
	}
	if err := s.qw.UpsertPRConversationFeedback(ctx, gen.UpsertPRConversationFeedbackParams{
		PRURL:             item.PRURL,
		CommentID:         item.CommentID,
		SessionID:         string(item.SessionID),
		SourceType:        sourceType,
		Author:            item.Author,
		Body:              item.Body,
		BodyHash:          item.BodyHash,
		URL:               item.URL,
		State:             state,
		Unverified:        boolInt(item.Unverified),
		AutoInjectReview:  boolInt(item.AutoInjectReview),
		CreatedAtProvider: nullTime(item.CreatedAtProvider),
		UpdatedAtProvider: nullTime(item.UpdatedAtProvider),
		ObservedAt:        item.ObservedAt.UTC(),
	}); err != nil {
		return fmt.Errorf("upsert pr conversation feedback %s/%s: %w", item.PRURL, item.CommentID, err)
	}
	return nil
}

func domainConversationFeedback(r gen.PRConversationFeedback) domain.PullRequestConversationFeedback {
	var createdAt, updatedAt time.Time
	if r.CreatedAtProvider.Valid {
		createdAt = r.CreatedAtProvider.Time.UTC()
	}
	if r.UpdatedAtProvider.Valid {
		updatedAt = r.UpdatedAtProvider.Time.UTC()
	}
	return domain.PullRequestConversationFeedback{
		PRURL:             r.PRURL,
		CommentID:         r.CommentID,
		SessionID:         domain.SessionID(r.SessionID),
		SourceType:        r.SourceType,
		Author:            r.Author,
		Body:              r.Body,
		BodyHash:          r.BodyHash,
		URL:               r.URL,
		State:             domain.PRConversationFeedbackState(r.State),
		Unverified:        r.Unverified != 0,
		AutoInjectReview:  r.AutoInjectReview != 0,
		CreatedAtProvider: createdAt,
		UpdatedAtProvider: updatedAt,
		ObservedAt:        r.ObservedAt.UTC(),
	}
}
