package sessionmanager

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessiontemp"
)

// SessionTempGCResult reports bounded cleanup work and per-directory decisions.
type SessionTempGCResult = sessiontemp.Result

// CollectSessionTemp performs one bounded session-temp collection pass. A
// dry-run uses the same retention, ownership, and liveness checks as deletion.
func (m *Manager) CollectSessionTemp(ctx context.Context, dryRun bool, limit int) (SessionTempGCResult, error) {
	if m.sessionTemps == nil {
		return SessionTempGCResult{}, fmt.Errorf("session temp storage is not configured")
	}
	var reserved []domain.SessionID
	defer func() {
		for _, id := range reserved {
			m.endAgentOperation(id, agentOperationTempGC)
		}
	}()
	return m.sessionTemps.Collect(ctx, m.clock(), limit, dryRun, func(ctx context.Context, owner sessiontemp.Owner) (sessiontemp.Eligibility, error) {
		decision, err := m.sessionTempEligibility(ctx, owner)
		if err != nil || !decision.Collect {
			return decision, err
		}
		id := domain.SessionID(owner.SessionID)
		if !m.tryBeginAgentOperation(id, agentOperationTempGC) {
			return sessiontemp.Eligibility{Reason: "session operation is in progress"}, nil
		}
		// Re-read durable state after reserving the session. A concurrent resume,
		// handoff, or cleanup either owns this gate already or will see it held.
		decision, err = m.sessionTempEligibility(ctx, owner)
		if err != nil || !decision.Collect {
			m.endAgentOperation(id, agentOperationTempGC)
			return decision, err
		}
		reserved = append(reserved, id)
		return decision, nil
	})
}

// ReconcileSessionTemp runs the daemon's bounded periodic orphan collection.
func (m *Manager) ReconcileSessionTemp(ctx context.Context) error {
	result, err := m.CollectSessionTemp(ctx, false, sessiontemp.DefaultBatchLimit)
	if err != nil {
		return err
	}
	for _, entry := range result.Entries {
		switch entry.Action {
		case "failed":
			m.logger.Warn("session temp cleanup failed",
				"sessionID", entry.SessionID, "reason", entry.Reason, "error", entry.Error, "bytes", entry.Bytes)
		case "skipped":
			if strings.Contains(entry.Reason, "unknown") || strings.Contains(entry.Reason, "failed") {
				m.logger.Debug("session temp cleanup retained directory",
					"sessionID", entry.SessionID, "reason", entry.Reason, "error", entry.Error)
			}
		}
	}
	if result.Removed > 0 {
		m.logger.Info("session temp cleanup reclaimed storage",
			"root", result.Root, "removed", result.Removed, "reclaimedBytes", result.ReclaimedBytes, "more", result.More)
	}
	return nil
}

func (m *Manager) sessionTempEligibility(ctx context.Context, owner sessiontemp.Owner) (sessiontemp.Eligibility, error) {
	rec, found, err := m.store.GetSession(ctx, domain.SessionID(owner.SessionID))
	if err != nil {
		return sessiontemp.Eligibility{}, err
	}
	if !found {
		if !owner.ExitedAt.IsZero() {
			return sessiontemp.Eligibility{Collect: true, ExitedAt: owner.ExitedAt, Reason: "session process exit was recorded before its row was removed"}, nil
		}
		return sessiontemp.Eligibility{Reason: "session row is missing and process exit was not recorded"}, nil
	}
	if !rec.IsTerminated {
		return sessiontemp.Eligibility{Reason: "session is active or resumable"}, nil
	}
	if rec.HibernatedAt != nil {
		return sessiontemp.Eligibility{Reason: "session is parked for resume"}, nil
	}
	mode := domain.NormalizeSessionMode(rec.Mode)
	if handleID := rec.Metadata.RuntimeHandleID; handleID != "" {
		if rec.Metadata.RuntimeLaunchID == "" || m.runtime == nil {
			return sessiontemp.Eligibility{Reason: "runtime generation or probe is unavailable"}, nil
		}
		probe := m.runtime.ProbeFencedRuntime(ctx, ports.FencedRuntimeRef{
			Handle:         ports.RuntimeHandle{ID: handleID},
			SessionID:      rec.ID,
			Generation:     rec.Metadata.RuntimeLaunchID,
			NativeIdentity: rec.Metadata.AgentSessionID,
		})
		if probe.Liveness != ports.FencedDead {
			return sessiontemp.Eligibility{Reason: fmt.Sprintf("runtime liveness is %s (%s)", probe.Liveness, probe.Reason)}, nil
		}
	} else if rec.Metadata.RuntimeLaunchID != "" && rec.Metadata.RuntimeReleasedLaunchID != rec.Metadata.RuntimeLaunchID {
		return sessiontemp.Eligibility{Reason: "runtime handle is missing without release evidence"}, nil
	} else if mode != domain.SessionModeChat && owner.ExitedAt.IsZero() &&
		(rec.Metadata.RuntimeLaunchID == "" || rec.Metadata.RuntimeReleasedLaunchID != rec.Metadata.RuntimeLaunchID) {
		return sessiontemp.Eligibility{Reason: "runtime handle is missing without confirmed exit evidence"}, nil
	}
	if mode == domain.SessionModeChat {
		if m.chat == nil {
			return sessiontemp.Eligibility{Reason: "chat controller liveness is unavailable"}, nil
		}
		if m.chat.HasLiveChatController(rec.ID) {
			return sessiontemp.Eligibility{Reason: "chat controller is still live"}, nil
		}
	}
	exitedAt := owner.ExitedAt
	if exitedAt.IsZero() {
		// The runtime/controller probe just confirmed exit. Use that point in
		// time so old terminal records cannot bypass the post-exit retention.
		exitedAt = m.clock()
	}
	return sessiontemp.Eligibility{Collect: true, ExitedAt: exitedAt, Reason: "session is terminal and its worker process is confirmed exited"}, nil
}

func (m *Manager) applySessionTempEnv(ctx context.Context, id domain.SessionID, env map[string]string) error {
	if m.sessionTemps == nil {
		return nil
	}
	dir, err := m.sessionTemps.Ensure(ctx, string(id))
	if err != nil {
		return err
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		setProtectedEnv(env, key, dir, envKeysCaseInsensitive)
	}
	return nil
}

func (m *Manager) applyExistingSessionTempEnv(ctx context.Context, id domain.SessionID, env map[string]string) error {
	if m.sessionTemps == nil {
		return nil
	}
	dir, found, err := m.sessionTemps.Lookup(ctx, string(id))
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		setProtectedEnv(env, key, dir, envKeysCaseInsensitive)
	}
	return nil
}

func (m *Manager) markSessionTempExited(ctx context.Context, id domain.SessionID) {
	if m.sessionTemps == nil {
		return
	}
	if err := m.sessionTemps.MarkExited(ctx, string(id)); err != nil {
		m.logger.Warn("record session temp process exit failed", "sessionID", id, "error", err)
	}
}

func (m *Manager) markSessionTempExitedIfConclusive(ctx context.Context, rec domain.SessionRecord) {
	if m.sessionTemps == nil {
		return
	}
	decision, err := m.sessionTempEligibility(ctx, sessiontemp.Owner{SessionID: string(rec.ID)})
	if err != nil {
		m.logger.Warn("check session temp process exit before session deletion", "sessionID", rec.ID, "error", err)
		return
	}
	if decision.Collect {
		m.markSessionTempExited(ctx, rec.ID)
	}
}
