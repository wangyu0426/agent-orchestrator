package sessionmanager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type mergeArchiveDefaults struct{ enabled bool }

func (d mergeArchiveDefaults) DefaultSessionMode(context.Context) domain.SessionMode {
	return domain.SessionModeTUI
}

func (d mergeArchiveDefaults) TerminateOnPRMergeDefault(context.Context) (bool, error) {
	return d.enabled, nil
}

func TestResolveTerminateOnPRMergePrecedencePreservesFalse(t *testing.T) {
	globalTrue := mergeArchiveDefaults{enabled: true}
	globalFalse := mergeArchiveDefaults{enabled: false}
	projectTrue := true
	projectFalse := false
	explicitTrue := true
	explicitFalse := false

	for _, tc := range []struct {
		name     string
		global   SessionMergePolicyDefaults
		project  domain.ProjectConfig
		explicit *bool
		want     bool
	}{
		{name: "global enabled by default", global: globalTrue, want: true},
		{name: "global explicit false", global: globalFalse, want: false},
		{name: "project false overrides global true", global: globalTrue, project: domain.ProjectConfig{TerminateOnPRMerge: &projectFalse}, want: false},
		{name: "project true overrides global false", global: globalFalse, project: domain.ProjectConfig{TerminateOnPRMerge: &projectTrue}, want: true},
		{name: "session false overrides project and global true", global: globalTrue, project: domain.ProjectConfig{TerminateOnPRMerge: &projectTrue}, explicit: &explicitFalse, want: false},
		{name: "session true overrides project and global false", global: globalFalse, project: domain.ProjectConfig{TerminateOnPRMerge: &projectFalse}, explicit: &explicitTrue, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _, _ := newManager()
			m.mergePolicyDefaults = tc.global
			got, err := m.resolveTerminateOnPRMerge(context.Background(), tc.explicit, tc.project)
			if err != nil || got != tc.want {
				t.Fatalf("resolved policy = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestWorkerSpawnPersistsResolvedArchiveOnMergePolicy(t *testing.T) {
	falseValue := false
	for _, tc := range []struct {
		name      string
		createdBy domain.SessionCreator
		global    bool
		project   *bool
		explicit  *bool
		want      bool
	}{
		{name: "tracker worker uses enabled global default", createdBy: domain.SessionCreatedByAutomation, global: true, want: true},
		{name: "manual override opts out", global: true, explicit: &falseValue, want: false},
		{name: "project false opts out of global default", global: true, project: &falseValue, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, store, _, _ := newManager()
			m.mergePolicyDefaults = mergeArchiveDefaults{enabled: tc.global}
			project := store.projects["mer"]
			project.Config.TerminateOnPRMerge = tc.project
			store.projects["mer"] = project
			rec, _, _, err := m.Spawn(context.Background(), ports.SpawnConfig{
				ProjectID: "mer", Kind: domain.KindWorker, CreatedBy: tc.createdBy,
				TerminateOnPRMerge: tc.explicit,
			})
			if err != nil {
				t.Fatal(err)
			}
			persisted := store.sessions[rec.ID]
			if rec.TerminateOnPRMerge != tc.want || persisted.TerminateOnPRMerge != tc.want {
				t.Fatalf("spawn policy = response %v, stored %v; want %v", rec.TerminateOnPRMerge, persisted.TerminateOnPRMerge, tc.want)
			}
		})
	}
}

type mergeArchiveTestStore struct {
	*fakeStore
	prs map[domain.SessionID][]domain.PullRequest
}

func (s *mergeArchiveTestStore) ListPRsBySession(_ context.Context, id domain.SessionID) ([]domain.PullRequest, error) {
	return append([]domain.PullRequest(nil), s.prs[id]...), nil
}

type mergeArchiveTestRuntime struct {
	*fakeRuntime
	commands   int
	commandErr error
}

func (r *mergeArchiveTestRuntime) RunningWorkloadCommands(context.Context, ports.RuntimeHandle, ports.SupervisedProcessRef) (int, error) {
	return r.commands, r.commandErr
}

type mergeArchiveTestWorkspace struct {
	*fakeWorkspace
	observation ports.WorkspaceObservation
	observeErr  error
}

func (w *mergeArchiveTestWorkspace) ObserveWorkspace(_ context.Context, info ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	if w.observeErr != nil {
		return ports.WorkspaceObservation{}, w.observeErr
	}
	observation := w.observation
	if observation.Path == "" {
		observation.Path = info.Path
	}
	return observation, nil
}

type mergeArchiveTestInputGate struct{}

func (mergeArchiveTestInputGate) BeginInputDrain(string) (time.Time, func()) {
	return time.Time{}, func() {}
}
func (mergeArchiveTestInputGate) LastInputAt(string) time.Time { return time.Time{} }
func (mergeArchiveTestInputGate) HoldInput(string) (time.Time, func()) {
	return time.Time{}, func() {}
}

type mergeArchiveFixture struct {
	manager   *Manager
	store     *mergeArchiveTestStore
	runtime   *mergeArchiveTestRuntime
	workspace *mergeArchiveTestWorkspace
	record    domain.SessionRecord
}

func newMergeArchiveFixture(t *testing.T, prs ...domain.PullRequest) mergeArchiveFixture {
	t.Helper()
	m, baseStore, baseRuntime, baseWorkspace := newManager()
	now := time.Now().UTC()
	id := domain.SessionID("mer-archive-worker")
	rec := domain.SessionRecord{
		ID: id, ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
		Mode:        domain.SessionModeTUI,
		Activity:    domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		DisplayName: "Keep this worker history",
		Metadata: domain.SessionMetadata{
			WorkspacePath: "/ws/" + string(id), Branch: "feature/archive-worker",
			RuntimeHandleID: string(id), RuntimeLaunchID: "launch-archive-worker",
			Prompt: "Preserve this recoverable task state",
		},
	}
	baseStore.projects["mer"] = domain.ProjectRecord{ID: "mer", Path: "/repo", Kind: domain.ProjectKindSingleRepo, Config: testRoleAgents()}
	baseStore.sessions[id] = rec
	tracked := make([]domain.PullRequest, len(prs))
	for i, pr := range prs {
		pr.SessionID = id
		if pr.ObservedAt.IsZero() {
			pr.ObservedAt = now
		}
		tracked[i] = pr
	}
	store := &mergeArchiveTestStore{fakeStore: baseStore, prs: map[domain.SessionID][]domain.PullRequest{id: tracked}}
	runtime := &mergeArchiveTestRuntime{fakeRuntime: baseRuntime}
	runtime.aliveByHandle = map[string]bool{string(id): true}
	workspace := &mergeArchiveTestWorkspace{fakeWorkspace: baseWorkspace}
	m.store = store
	m.runtime = runtime
	m.workspace = workspace
	m.SetTerminalInputGate(mergeArchiveTestInputGate{})
	m.killTeardown = time.Second
	return mergeArchiveFixture{manager: m, store: store, runtime: runtime, workspace: workspace, record: rec}
}

func mergedPR(number int) domain.PullRequest {
	return domain.PullRequest{Number: number, URL: "https://example.test/repo/pull/" + string(rune('0'+number)), Merged: true, Closed: true, ObservedAt: time.Now().UTC()}
}

func TestArchiveMergedSessionDefersUnsafeStatesAndUnknownOwnership(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*mergeArchiveFixture)
		reason string
	}{
		{name: "active worker", setup: func(f *mergeArchiveFixture) {
			rec := f.store.sessions[f.record.ID]
			rec.Activity.State = domain.ActivityActive
			f.store.sessions[f.record.ID] = rec
		}, reason: "worker_not_idle"},
		{name: "another PR still open", setup: func(f *mergeArchiveFixture) {
			f.store.prs[f.record.ID] = append(f.store.prs[f.record.ID], domain.PullRequest{Number: 2, URL: "https://example.test/repo/pull/2", ObservedAt: time.Now().UTC()})
		}, reason: "another_pull_request_open_or_unknown"},
		{name: "closed without merge", setup: func(f *mergeArchiveFixture) {
			f.store.prs[f.record.ID][0] = domain.PullRequest{Number: 1, URL: "https://example.test/repo/pull/1", Closed: true, ObservedAt: time.Now().UTC()}
		}, reason: "no_authoritative_merged_pull_request"},
		{name: "PR state unknown", setup: func(f *mergeArchiveFixture) { f.store.prs[f.record.ID][0].ObservedAt = time.Time{} }, reason: "pull_request_state_unknown"},
		{name: "runtime ownership unknown", setup: func(f *mergeArchiveFixture) {
			rec := f.store.sessions[f.record.ID]
			rec.Metadata.RuntimeLaunchID = ""
			f.store.sessions[f.record.ID] = rec
		}, reason: "runtime_process_ownership_unknown"},
		{name: "runtime liveness unknown", setup: func(f *mergeArchiveFixture) {
			f.runtime.fencedResult = ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonProbeFailed}
		}, reason: "runtime_process_liveness_unknown"},
		{name: "workspace state unknown", setup: func(f *mergeArchiveFixture) { f.workspace.observeErr = errors.New("workspace probe failed") }, reason: "workspace_state_unknown"},
		{name: "workspace dirty", setup: func(f *mergeArchiveFixture) { f.workspace.observation.Dirty = true }, reason: "workspace_has_unpreserved_work"},
		{name: "workload process active", setup: func(f *mergeArchiveFixture) { f.runtime.commands = 1 }, reason: "terminal_workload_running"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newMergeArchiveFixture(t, mergedPR(1))
			tc.setup(&f)
			_, err := f.manager.ArchiveMergedSession(context.Background(), f.record.ID)
			var deferred *MergeArchiveDeferredError
			if !errors.As(err, &deferred) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("archive error = %v; want deferred reason %q", err, tc.reason)
			}
			if f.runtime.destroyed != 0 || f.store.sessions[f.record.ID].IsTerminated {
				t.Fatalf("unsafe archive released resources: destroys=%d session=%+v", f.runtime.destroyed, f.store.sessions[f.record.ID])
			}
		})
	}
}

func TestArchiveMergedSessionRetriesFailedOwnedProcessTeardownAndIsIdempotent(t *testing.T) {
	f := newMergeArchiveFixture(t, mergedPR(1))
	f.runtime.destroyErr = errors.New("runtime destroy failed")
	if _, err := f.manager.ArchiveMergedSession(context.Background(), f.record.ID); err == nil {
		t.Fatal("failed owned process termination was reported as success")
	}
	if f.store.sessions[f.record.ID].IsTerminated || f.runtime.destroyed != 1 {
		t.Fatalf("failed termination changed session or process state: session=%+v destroys=%d", f.store.sessions[f.record.ID], f.runtime.destroyed)
	}
	f.runtime.destroyErr = nil
	restarted := New(Deps{
		Runtime: f.runtime, Agents: fakeAgents{}, Workspace: f.workspace, Store: f.store,
		Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: f.store.fakeStore},
		LookPath: func(string) (string, error) { return "/bin/true", nil },
	})
	restarted.SetTerminalInputGate(mergeArchiveTestInputGate{})
	restarted.killTeardown = time.Second
	freed, err := restarted.ArchiveMergedSession(context.Background(), f.record.ID)
	if err != nil || !freed {
		t.Fatalf("retry archive = freed %v, err %v; want released workspace", freed, err)
	}
	archived := f.store.sessions[f.record.ID]
	if !archived.IsTerminated || archived.DisplayName != f.record.DisplayName || archived.Metadata.Prompt != f.record.Metadata.Prompt {
		t.Fatalf("archive did not preserve history metadata: %+v", archived)
	}
	if f.runtime.destroyed != 2 || len(f.runtime.destroyedIDs) != 2 || f.runtime.destroyedIDs[1] != string(f.record.ID) {
		t.Fatalf("owned runtime teardown calls = %v", f.runtime.destroyedIDs)
	}
	freed, err = restarted.ArchiveMergedSession(context.Background(), f.record.ID)
	if err != nil || freed || f.runtime.destroyed != 2 {
		t.Fatalf("repeat archive = freed %v, err %v, destroys %d; want idempotent no-op", freed, err, f.runtime.destroyed)
	}
}

func TestArchiveMergedSessionRequiresConfirmedExitForExitedWorker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		alive   bool
		wantErr string
	}{
		{name: "exact process exit confirmed", alive: false},
		{name: "durable exit conflicts with live process", alive: true, wantErr: "runtime_exit_not_confirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMergeArchiveFixture(t, mergedPR(1))
			rec := f.store.sessions[f.record.ID]
			rec.Activity.State = domain.ActivityExited
			f.store.sessions[f.record.ID] = rec
			f.runtime.aliveByHandle[string(f.record.ID)] = tc.alive
			freed, err := f.manager.ArchiveMergedSession(context.Background(), f.record.ID)
			if tc.wantErr != "" {
				if !errors.As(err, new(*MergeArchiveDeferredError)) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("archive error = %v; want reason %q", err, tc.wantErr)
				}
				if f.runtime.destroyed != 0 || f.store.sessions[f.record.ID].IsTerminated {
					t.Fatal("live contradictory process was terminated")
				}
				return
			}
			if err != nil || !freed || !f.store.sessions[f.record.ID].IsTerminated {
				t.Fatalf("archive confirmed exit = freed %v, err %v, record %+v", freed, err, f.store.sessions[f.record.ID])
			}
		})
	}
}

func TestMergedArchivePreviewAndExplicitBatchApply(t *testing.T) {
	f := newMergeArchiveFixture(t, mergedPR(1), domain.PullRequest{Number: 2, URL: "https://example.test/repo/pull/2", Closed: true, ObservedAt: time.Now().UTC()})
	candidates, err := f.manager.PreviewMergedArchive(context.Background(), "mer")
	if err != nil || len(candidates) != 1 || !candidates[0].Eligible {
		t.Fatalf("preview = %+v, %v; want one eligible merged worker", candidates, err)
	}
	results := f.manager.ApplyMergedArchive(context.Background(), []domain.SessionID{f.record.ID, f.record.ID})
	if len(results) != 1 || !results[0].Archived || !results[0].WorkspaceFreed {
		t.Fatalf("explicit batch apply = %+v; want one idempotently deduplicated archive", results)
	}
}
