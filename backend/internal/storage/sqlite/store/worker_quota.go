package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

// WorkerQuotaSessions lists enrolled workers that still own their sessions.
func (s *Store) WorkerQuotaSessions(ctx context.Context) ([]workerquota.Session, error) {
	rows, err := s.qr.ListWorkerQuotaSessions(ctx)
	if err != nil {
		return nil, err
	}
	sessions := make([]workerquota.Session, 0, len(rows))
	for _, r := range rows {
		var remaining []workerquota.Target
		if err := json.Unmarshal([]byte(r.RemainingRoutes), &remaining); err != nil {
			return nil, fmt.Errorf("read remaining quota routes for session %s: %w", r.SessionID, err)
		}
		sessions = append(sessions, workerquota.Session{
			ID: domain.SessionID(r.SessionID), Initial: workerquota.Target{Harness: domain.AgentHarness(r.Harness), Model: r.Model, Effort: r.Effort},
			Remaining: remaining,
			Target:    workerquota.Target{Harness: domain.AgentHarness(r.TargetHarness), Model: r.TargetModel},
			Attempt:   r.AttemptOwner, Reason: r.Reason, FailureAt: time.UnixMilli(r.FailureAt),
		})
	}
	return sessions, nil
}

// ClaimWorkerQuotaAttempt atomically reserves the worker's automatic handoff.
func (s *Store) ClaimWorkerQuotaAttempt(ctx context.Context, id domain.SessionID, attempt string, target workerquota.Target) (bool, error) {
	rows, err := s.qw.ClaimWorkerQuotaAttempt(ctx, gen.ClaimWorkerQuotaAttemptParams{
		SessionID: string(id), AttemptOwner: attempt, TargetHarness: string(target.Harness), TargetModel: target.Model,
	})
	return rows != 0, err
}

// SetWorkerQuotaReason records a diagnostic and reports whether it changed.
func (s *Store) SetWorkerQuotaReason(ctx context.Context, id domain.SessionID, reason string) (bool, error) {
	rows, err := s.qw.SetWorkerQuotaReason(ctx, gen.SetWorkerQuotaReasonParams{SessionID: string(id), Reason: reason})
	return rows != 0, err
}

// RecordWorkerQuotaFailure persists the latest native refusal for admission.
func (s *Store) RecordWorkerQuotaFailure(ctx context.Context, id domain.SessionID, f workerquota.Failure) error {
	var reset int64
	if !f.ResetAt.IsZero() && f.Scope != "" {
		reset = f.ResetAt.UnixMilli()
	}
	return s.qw.RecordWorkerQuotaFailure(ctx, gen.RecordWorkerQuotaFailureParams{
		SessionID: string(id), FailureAt: f.At.UnixMilli(), ResetAt: reset,
		FailureHarness: string(f.Target.Harness), FailureModel: f.Target.Model,
	})
}

// WorkerQuotaFailures returns evidence whose reset deadline has not passed.
func (s *Store) WorkerQuotaFailures(ctx context.Context, now time.Time) ([]workerquota.Failure, error) {
	rows, err := s.qr.ListWorkerQuotaFailures(ctx, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	failures := make([]workerquota.Failure, 0, len(rows))
	for _, r := range rows {
		failures = append(failures, workerquota.Failure{
			Target: workerquota.Target{Harness: domain.AgentHarness(r.FailureHarness), Model: r.FailureModel},
			At:     time.UnixMilli(r.FailureAt), ResetAt: time.UnixMilli(r.ResetAt), Scope: "computer login",
		})
	}
	return failures, nil
}
