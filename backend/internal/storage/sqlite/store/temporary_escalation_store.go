package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// Temporary model escalation persistence (issue #8). The record is one
// JSON blob on the sessions row; its state is mirrored in a column so
// transitions can CAS on state. All writers take writeMu, matching the
// other session writers.

func encodeEscalation(esc *domain.TemporaryEscalation) (sql.NullString, error) {
	raw, err := json.Marshal(esc)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encode temporary escalation for %s: %w", esc.SessionID, err)
	}
	return sql.NullString{String: string(raw), Valid: true}, nil
}

func decodeEscalation(id domain.SessionID, raw sql.NullString) (*domain.TemporaryEscalation, bool, error) {
	if !raw.Valid || raw.String == "" {
		return nil, false, nil
	}
	var esc domain.TemporaryEscalation
	if err := json.Unmarshal([]byte(raw.String), &esc); err != nil {
		return nil, false, fmt.Errorf("decode temporary escalation for %s: %w", id, err)
	}
	if err := esc.Validate(); err != nil {
		return nil, false, fmt.Errorf("decode temporary escalation for %s: %w", id, err)
	}
	return &esc, true, nil
}

func stateParam(state domain.TemporaryEscalationState) sql.NullString {
	return sql.NullString{String: string(state), Valid: true}
}

// GetEscalation returns the latest escalation for a session, terminal or
// not. found is false when the session never had one.
func (s *Store) GetEscalation(ctx context.Context, id domain.SessionID) (*domain.TemporaryEscalation, bool, error) {
	row, err := s.qr.GetTemporaryEscalation(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get temporary escalation for %s: %w", id, err)
	}
	return decodeEscalation(id, row.TemporaryEscalation)
}

// CreateEscalation claims the escalation slot. It affects no row when an
// active or failed escalation already exists for the session; the caller
// then reads back the winner.
func (s *Store) CreateEscalation(ctx context.Context, esc *domain.TemporaryEscalation) error {
	if err := esc.Validate(); err != nil {
		return err
	}
	raw, err := encodeEscalation(esc)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.CreateTemporaryEscalation(ctx, gen.CreateTemporaryEscalationParams{
		TemporaryEscalation:      raw,
		TemporaryEscalationState: stateParam(esc.State),
		UpdatedAt:                esc.UpdatedAt,
		ID:                       esc.SessionID,
	})
	if err != nil {
		return fmt.Errorf("create temporary escalation for %s: %w", esc.SessionID, err)
	}
	if rows == 0 {
		return fmt.Errorf("create temporary escalation for %s: an active escalation already exists", esc.SessionID)
	}
	return nil
}

// UpdateEscalation is a compare-and-swap on the previous state: it
// reports false when another writer moved the record first.
func (s *Store) UpdateEscalation(ctx context.Context, esc *domain.TemporaryEscalation, from domain.TemporaryEscalationState) (bool, error) {
	if err := esc.Validate(); err != nil {
		return false, err
	}
	raw, err := encodeEscalation(esc)
	if err != nil {
		return false, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.UpdateTemporaryEscalation(ctx, gen.UpdateTemporaryEscalationParams{
		TemporaryEscalation:        raw,
		TemporaryEscalationState:   stateParam(esc.State),
		UpdatedAt:                  esc.UpdatedAt,
		ID:                         esc.SessionID,
		TemporaryEscalationState_2: stateParam(from),
	})
	if err != nil {
		return false, fmt.Errorf("update temporary escalation for %s: %w", esc.SessionID, err)
	}
	return rows > 0, nil
}

// DeleteEscalation clears the escalation record, used when an upgrade
// switch fails and no auto-restore must ever fire for it.
func (s *Store) DeleteEscalation(ctx context.Context, id domain.SessionID) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.qw.DeleteTemporaryEscalation(ctx, id); err != nil {
		return fmt.Errorf("delete temporary escalation for %s: %w", id, err)
	}
	return nil
}

// ListActiveEscalations returns every escalation that still needs
// attention: pending, awaiting idle, reverting, or failed.
func (s *Store) ListActiveEscalations(ctx context.Context) ([]*domain.TemporaryEscalation, error) {
	rows, err := s.qr.ListActiveTemporaryEscalations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active temporary escalations: %w", err)
	}
	out := make([]*domain.TemporaryEscalation, 0, len(rows))
	for _, row := range rows {
		esc, found, err := decodeEscalation(row.ID, row.TemporaryEscalation)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		out = append(out, esc)
	}
	return out, nil
}
