package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type mergeArchivePRStore interface {
	ListPRsBySession(context.Context, domain.SessionID) ([]domain.PullRequest, error)
}

// MergeArchiveDeferredError records why an automatic or explicit merge archive
// was deferred. Callers can retry after the underlying state changes.
type MergeArchiveDeferredError struct {
	SessionID domain.SessionID
	Reason    string
	Cause     error
}

func (e *MergeArchiveDeferredError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("archive merged session %s deferred (%s): %v", e.SessionID, e.Reason, e.Cause)
	}
	return fmt.Sprintf("archive merged session %s deferred (%s)", e.SessionID, e.Reason)
}

func (e *MergeArchiveDeferredError) Unwrap() error { return e.Cause }

// MergeArchiveCandidate is one session shown by the explicit batch preview.
type MergeArchiveCandidate struct {
	SessionID   domain.SessionID
	ProjectID   domain.ProjectID
	DisplayName string
	Eligible    bool
	Reason      string
}

// MergeArchiveApplyResult is one idempotent result from an explicit batch apply.
type MergeArchiveApplyResult struct {
	SessionID      domain.SessionID
	Archived       bool
	WorkspaceFreed bool
	Reason         string
}

// PreviewMergedArchive lists policy-independent, currently merged worker
// sessions and reports which ones pass the same gates used by apply.
func (m *Manager) PreviewMergedArchive(ctx context.Context, projectID domain.ProjectID) ([]MergeArchiveCandidate, error) {
	recs, err := m.store.ListAllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions for merge archive preview: %w", err)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	var out []MergeArchiveCandidate
	for _, rec := range recs {
		if rec.Kind != domain.KindWorker || rec.IsTerminated || (projectID != "" && rec.ProjectID != projectID) {
			continue
		}
		prs, err := m.listMergeArchivePRs(ctx, rec.ID)
		if err != nil {
			return nil, fmt.Errorf("preview merge archive for %s: read pull requests: %w", rec.ID, err)
		}
		merged := false
		for _, pr := range prs {
			merged = merged || pr.Merged
		}
		if !merged {
			continue
		}
		rows, workspaceProjectRows, err := m.workspaceProjectRows(ctx, rec)
		if err != nil {
			out = append(out, MergeArchiveCandidate{SessionID: rec.ID, ProjectID: rec.ProjectID, DisplayName: rec.DisplayName, Reason: "workspace_state_unknown"})
			m.logger.Warn("merge archive preview deferred", "sessionID", rec.ID, "reason", "workspace_state_unknown", "error", err)
			continue
		}
		_, reason, cause := m.mergeArchiveEligibility(ctx, rec, rows, workspaceProjectRows, false)
		if cause != nil {
			m.logger.Warn("merge archive preview deferred", "sessionID", rec.ID, "reason", reason, "error", cause)
		}
		out = append(out, MergeArchiveCandidate{
			SessionID: rec.ID, ProjectID: rec.ProjectID, DisplayName: rec.DisplayName,
			Eligible: reason == "", Reason: reason,
		})
	}
	return out, nil
}

// ApplyMergedArchive applies only the explicit session ids supplied by the
// caller. Every session is rechecked under its operation lock before teardown.
func (m *Manager) ApplyMergedArchive(ctx context.Context, ids []domain.SessionID) []MergeArchiveApplyResult {
	out := make([]MergeArchiveApplyResult, 0, len(ids))
	seen := make(map[domain.SessionID]struct{}, len(ids))
	for _, id := range ids {
		id = domain.SessionID(strings.TrimSpace(string(id)))
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		freed, err := m.ArchiveMergedSession(ctx, id)
		result := MergeArchiveApplyResult{SessionID: id, Archived: err == nil, WorkspaceFreed: freed}
		if err != nil {
			result.Reason = err.Error()
			m.logger.Warn("merge archive apply failed", "sessionID", id, "error", err)
		}
		out = append(out, result)
	}
	return out
}

func (m *Manager) listMergeArchivePRs(ctx context.Context, id domain.SessionID) ([]domain.PullRequest, error) {
	store, ok := m.store.(mergeArchivePRStore)
	if !ok {
		return nil, errors.New("pull request store is unavailable")
	}
	return store.ListPRsBySession(ctx, id)
}

// mergeArchiveEligibility uses provider facts, exact AO ownership evidence,
// idle state, a clean workspace snapshot, and the existing workload guard. A
// releaseInput result holds PTY input admission through teardown when requested.
func (m *Manager) mergeArchiveEligibility(
	ctx context.Context,
	rec domain.SessionRecord,
	workspaceRows []ports.WorkspaceRepoInfo,
	workspaceProject bool,
	holdInput bool,
) (releaseInput func(), reason string, cause error) {
	defer func() {
		if reason != "" && releaseInput != nil {
			releaseInput()
			releaseInput = nil
		}
	}()
	if rec.Kind != domain.KindWorker {
		return nil, "not_a_worker", nil
	}
	if rec.IsTerminated {
		return nil, "already_archived", nil
	}
	if rec.ProvisionState.IsProvisioning() {
		return nil, "worker_starting", nil
	}
	activityExited := rec.Activity.State == domain.ActivityExited
	if rec.Activity.State != domain.ActivityIdle && !activityExited {
		return nil, "worker_not_idle", nil
	}
	if active, err := m.hasActiveInterfaceTransition(ctx, rec.ID); err != nil {
		return nil, "interface_handoff_unknown", err
	} else if active {
		return nil, "interface_handoff_active", nil
	}

	prs, err := m.listMergeArchivePRs(ctx, rec.ID)
	if err != nil {
		return nil, "pull_request_state_unknown", err
	}
	merged := false
	if len(prs) == 0 {
		return nil, "no_tracked_pull_requests", nil
	}
	for _, pr := range prs {
		if pr.ObservedAt.IsZero() {
			return nil, "pull_request_state_unknown", nil
		}
		if pr.Merged {
			merged = true
		} else if !pr.Closed {
			return nil, "another_pull_request_open_or_unknown", nil
		}
	}
	if !merged {
		return nil, "no_authoritative_merged_pull_request", nil
	}

	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		if m.chat == nil {
			return nil, "chat_process_ownership_unknown", nil
		}
		if !m.chat.HasLiveChatController(rec.ID) && rec.HibernatedAt == nil {
			return nil, "chat_process_ownership_unknown", nil
		}
	} else {
		handle := runtimeHandle(rec.Metadata)
		launchID := strings.TrimSpace(rec.Metadata.RuntimeLaunchID)
		if handle.ID == "" || handle.ID != string(rec.ID) || launchID == "" {
			return nil, "runtime_process_ownership_unknown", nil
		}
		if holdInput {
			_, release := m.fenceTerminalInput(rec)
			if release == nil {
				return nil, "terminal_input_ownership_unknown", nil
			}
			releaseInput = release
		} else {
			gate, terminalID := m.terminalInputGateFor(rec)
			if _, ok := gate.(TerminalInputObserver); !ok || terminalID == "" {
				return nil, "terminal_input_ownership_unknown", nil
			}
		}
		current, found, err := m.store.GetSession(ctx, rec.ID)
		if err != nil {
			return nil, "session_state_unknown", err
		}
		if !found || current.IsTerminated || current.Activity.State != rec.Activity.State ||
			(current.Activity.State != domain.ActivityIdle && current.Activity.State != domain.ActivityExited) ||
			current.Metadata.RuntimeLaunchID != launchID {
			return nil, "worker_state_changed", nil
		}
		lastInputAt := m.lastTerminalInputAt(current)
		if !activityExited && !m.terminalIdleForRestart(ctx, current, lastInputAt) {
			return nil, "terminal_not_idle", nil
		}
		probe := m.runtime.ProbeFencedRuntime(ctx, ports.FencedRuntimeRef{
			Handle: handle, SessionID: rec.ID, Generation: launchID,
			NativeIdentity: rec.Metadata.AgentSessionID,
		})
		if probe.Liveness == ports.FencedUnknown {
			return nil, "runtime_process_liveness_unknown", fmt.Errorf("exact runtime probe: %s", probe.Reason)
		}
		if activityExited && probe.Liveness != ports.FencedDead {
			return nil, "runtime_exit_not_confirmed", nil
		}
		inspector, ok := m.runtime.(ports.WorkloadCommandInspector)
		if !ok {
			return nil, "workload_process_ownership_unknown", nil
		}
		commands, err := inspector.RunningWorkloadCommands(ctx, handle, ports.SupervisedProcessRef{SessionID: rec.ID, LaunchID: launchID})
		if err != nil {
			return nil, "workload_process_liveness_unknown", err
		}
		if commands > 0 {
			return nil, "terminal_workload_running", nil
		}
	}

	observer, ok := m.workspace.(ports.WorkspaceObserver)
	if !ok {
		return nil, "workspace_state_unknown", nil
	}
	infos := make([]ports.WorkspaceInfo, 0, len(workspaceRows))
	if workspaceProject {
		if len(workspaceRows) == 0 {
			return nil, "workspace_state_unknown", nil
		}
		for _, row := range workspaceRows {
			infos = append(infos, workspaceInfoFromRepoInfo(row))
		}
	} else {
		info := workspaceInfo(rec)
		if strings.TrimSpace(info.Path) == "" {
			return nil, "workspace_state_unknown", nil
		}
		infos = append(infos, info)
	}
	project, err := m.loadProject(ctx, rec.ProjectID)
	if err != nil {
		return nil, "workspace_state_unknown", err
	}
	if project.Kind.WithDefault() == domain.ProjectKindScratch {
		return nil, "workspace_state_unknown", nil
	}
	for _, info := range infos {
		observation, err := observer.ObserveWorkspace(ctx, info)
		if err != nil {
			return nil, "workspace_state_unknown", err
		}
		if observation.Dirty || observation.Staged || observation.Untracked || len(observation.Changes) != 0 {
			return nil, "workspace_has_unpreserved_work", nil
		}
	}
	latest, found, err := m.store.GetSession(ctx, rec.ID)
	if err != nil {
		return nil, "session_state_unknown", err
	}
	if !found || latest.IsTerminated || latest.Activity.State != rec.Activity.State ||
		latest.Metadata.RuntimeLaunchID != rec.Metadata.RuntimeLaunchID ||
		latest.Metadata.RuntimeHandleID != rec.Metadata.RuntimeHandleID {
		return nil, "worker_state_changed", nil
	}
	return releaseInput, "", nil
}
