package sessionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessiontemp"
)

func TestSessionTempEligibilityFailsClosed(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	parkedAt := now.Add(-time.Hour)
	tests := []struct {
		name        string
		record      domain.SessionRecord
		runtime     *fakeRuntime
		chat        ChatLauncher
		owner       sessiontemp.Owner
		wantCollect bool
		wantReason  string
		wantExit    time.Time
	}{
		{
			name:       "active session",
			record:     domain.SessionRecord{ID: "session-active", Mode: domain.SessionModeTUI, IsTerminated: false},
			wantReason: "active or resumable",
		},
		{
			name:       "parked session",
			record:     domain.SessionRecord{ID: "session-parked", Mode: domain.SessionModeChat, IsTerminated: true, HibernatedAt: &parkedAt},
			chat:       &recordingLauncher{},
			wantReason: "parked for resume",
		},
		{
			name:       "unknown TUI liveness",
			record:     domain.SessionRecord{ID: "session-unknown", Mode: domain.SessionModeTUI, IsTerminated: true, Metadata: domain.SessionMetadata{RuntimeHandleID: "handle", RuntimeLaunchID: "launch"}},
			runtime:    &fakeRuntime{fencedResult: ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonProbeFailed}},
			wantReason: "runtime liveness is unknown",
		},
		{
			name:       "live TUI process",
			record:     domain.SessionRecord{ID: "session-live", Mode: domain.SessionModeTUI, IsTerminated: true, Metadata: domain.SessionMetadata{RuntimeHandleID: "handle", RuntimeLaunchID: "launch"}},
			runtime:    &fakeRuntime{fencedResult: ports.FencedProbeResult{Liveness: ports.FencedAlive, Reason: ports.FencedReasonExactMatch}},
			wantReason: "runtime liveness is alive",
		},
		{
			name:        "dead TUI process",
			record:      domain.SessionRecord{ID: "session-dead", Mode: domain.SessionModeTUI, IsTerminated: true, UpdatedAt: now.Add(-30 * 24 * time.Hour), Metadata: domain.SessionMetadata{RuntimeHandleID: "handle", RuntimeLaunchID: "launch"}},
			runtime:     &fakeRuntime{fencedResult: ports.FencedProbeResult{Liveness: ports.FencedDead, Reason: ports.FencedReasonExactAbsent}},
			wantCollect: true,
			wantExit:    now,
		},
		{
			name:       "missing TUI handle without evidence",
			record:     domain.SessionRecord{ID: "session-no-handle", Mode: domain.SessionModeTUI, IsTerminated: true},
			wantReason: "runtime handle is missing without confirmed exit evidence",
		},
		{
			name:       "chat controller liveness unavailable",
			record:     domain.SessionRecord{ID: "session-chat-unknown", Mode: domain.SessionModeChat, IsTerminated: true},
			wantReason: "chat controller liveness is unavailable",
		},
		{
			name:       "live chat controller",
			record:     domain.SessionRecord{ID: "session-chat-live", Mode: domain.SessionModeChat, IsTerminated: true},
			chat:       &recordingLauncher{live: true},
			wantReason: "chat controller is still live",
		},
		{
			name:        "stopped chat controller",
			record:      domain.SessionRecord{ID: "session-chat-dead", Mode: domain.SessionModeChat, IsTerminated: true, UpdatedAt: now.Add(-30 * 24 * time.Hour)},
			chat:        &recordingLauncher{},
			wantCollect: true,
			wantExit:    now,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			store.sessions[tt.record.ID] = tt.record
			manager := New(Deps{Store: store, Runtime: tt.runtime, Chat: tt.chat, Clock: func() time.Time { return now }})
			decision, err := manager.sessionTempEligibility(context.Background(), sessiontemp.Owner{SessionID: string(tt.record.ID)})
			if err != nil {
				t.Fatal(err)
			}
			if decision.Collect != tt.wantCollect {
				t.Fatalf("Collect = %v, want %v; decision=%+v", decision.Collect, tt.wantCollect, decision)
			}
			if tt.wantReason != "" && !strings.Contains(decision.Reason, tt.wantReason) {
				t.Fatalf("reason = %q, want it to contain %q", decision.Reason, tt.wantReason)
			}
			if tt.wantCollect && !decision.ExitedAt.Equal(tt.wantExit) {
				t.Fatalf("exit time = %s, want confirmed time %s", decision.ExitedAt, tt.wantExit)
			}
		})
	}
}

func TestLaunchRuntimeEnvUsesPrivateSessionTempAcrossHandoffs(t *testing.T) {
	manager := New(Deps{
		TempRoot:      filepath.Join(t.TempDir(), "ao", "tmp"),
		TempRetention: time.Hour,
		DataDir:       t.TempDir(),
		Executable:    func() (string, error) { return filepath.Join("/opt", "ao", "ao"), nil },
	})
	firstEnv, _, err := manager.launchRuntimeEnv(context.Background(), "session-handoff", "project-1", "", map[string]string{"TMPDIR": "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	secondEnv := map[string]string{}
	if err := manager.applySessionTempEnv(context.Background(), "session-handoff", secondEnv); err != nil {
		t.Fatal(err)
	}
	firstDir := firstEnv["TMPDIR"]
	if firstDir == "" || firstDir == "/tmp" {
		t.Fatalf("TMPDIR = %q, expected private session path", firstDir)
	}
	if got := firstEnv["TMP"]; got != firstDir {
		t.Errorf("TMP = %q, want %q", got, firstDir)
	}
	if got := firstEnv["TEMP"]; got != firstDir {
		t.Errorf("TEMP = %q, want %q", got, firstDir)
	}
	if secondEnv["TMPDIR"] != firstDir || secondEnv["TMP"] != firstDir || secondEnv["TEMP"] != firstDir {
		t.Fatalf("handoff environment changed session temp path: first=%v second=%v", firstEnv, secondEnv)
	}
}

func TestTerminalSpawnPassesSessionTempEnvToRuntime(t *testing.T) {
	manager, _, runtime, _ := newManager()
	manager.sessionTemps = sessiontemp.New(privateSessionTempRoot(t), time.Hour)

	rec, _, _, err := manager.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: "mer", Kind: domain.KindWorker,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir, found, err := manager.sessionTemps.Lookup(context.Background(), string(rec.ID))
	if err != nil || !found {
		t.Fatalf("session temp directory lookup found=%v err=%v", found, err)
	}
	assertSessionTempEnv(t, runtime.lastCfg.Env, dir)
}

func TestChatSpawnPassesSessionTempEnvToController(t *testing.T) {
	launcher := &recordingLauncher{}
	manager, _, runtime := newChatManager(launcher)
	manager.sessionTemps = sessiontemp.New(privateSessionTempRoot(t), time.Hour)

	rec, _, _, err := manager.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		RequestedMode: domain.SessionModeChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.started) != 1 {
		t.Fatalf("chat controller launches = %d, want 1", len(launcher.started))
	}
	dir, found, err := manager.sessionTemps.Lookup(context.Background(), string(rec.ID))
	if err != nil || !found {
		t.Fatalf("session temp directory lookup found=%v err=%v", found, err)
	}
	assertSessionTempEnv(t, launcher.started[0].Env, dir)
	if runtime.created != 0 {
		t.Fatalf("chat spawn created %d terminal runtimes, want none", runtime.created)
	}
}

func TestChatStopFailureDoesNotAuthorizeTempGC(t *testing.T) {
	launcher := &recordingLauncher{turnErr: errors.New("initial turn failed"), stopErr: errors.New("controller still stopping"), live: true}
	manager, store, _ := newChatManager(launcher)
	manager.sessionTemps = sessiontemp.New(privateSessionTempRoot(t), time.Nanosecond)
	manager.clock = func() time.Time { return time.Now().UTC().Add(time.Hour) }

	_, _, _, err := manager.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		RequestedMode: domain.SessionModeChat, Prompt: "run the first turn",
	})
	if err == nil {
		t.Fatal("chat spawn with a failed initial turn unexpectedly succeeded")
	}
	if len(launcher.stopped) != 1 {
		t.Fatalf("StopChat calls = %d, want 1", len(launcher.stopped))
	}

	dir, found, lookupErr := manager.sessionTemps.Lookup(context.Background(), "mer-1")
	if lookupErr != nil || !found {
		t.Fatalf("session temp directory lookup found=%v err=%v", found, lookupErr)
	}
	data, readErr := os.ReadFile(filepath.Join(dir, ".ao-session-temp.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var owner sessiontemp.Owner
	if err := json.Unmarshal(data, &owner); err != nil {
		t.Fatal(err)
	}
	if !owner.ExitedAt.IsZero() {
		t.Fatalf("failed Chat stop recorded process exit: %+v", owner)
	}

	result, err := manager.CollectSessionTemp(context.Background(), false, 8)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 0 || len(result.Entries) != 1 || result.Entries[0].Action != "skipped" || store.sessions["mer-1"].IsTerminated == false {
		t.Fatalf("failed Chat stop was not retained as live: result=%+v session=%+v", result, store.sessions["mer-1"])
	}
}

func assertSessionTempEnv(t *testing.T, env map[string]string, want string) {
	t.Helper()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if got := env[key]; got != want {
			t.Errorf("worker %s = %q, want private session path %q", key, got, want)
		}
	}
}

func TestCollectSessionTempSkipsConcurrentSessionOperation(t *testing.T) {
	now := time.Now().UTC()
	clockNow := now.Add(-2 * time.Hour)
	store := newFakeStore()
	store.sessions["session-gc"] = domain.SessionRecord{
		ID: "session-gc", Mode: domain.SessionModeTUI, IsTerminated: true,
		Metadata: domain.SessionMetadata{RuntimeHandleID: "handle", RuntimeLaunchID: "launch"},
	}
	manager := New(Deps{
		Store: store, Runtime: &fakeRuntime{fencedResult: ports.FencedProbeResult{Liveness: ports.FencedDead, Reason: ports.FencedReasonExactAbsent}},
		TempRoot: privateSessionTempRoot(t), TempRetention: time.Nanosecond, Clock: func() time.Time { return clockNow },
	})
	dir, err := manager.sessionTemps.Ensure(context.Background(), "session-gc")
	if err != nil {
		t.Fatal(err)
	}
	clockNow = time.Now().UTC().Add(time.Second)
	if err := manager.beginAgentOperation(context.Background(), "session-gc", agentOperationResume); err != nil {
		t.Fatal(err)
	}
	result, err := manager.CollectSessionTemp(context.Background(), false, 8)
	manager.endAgentOperation("session-gc", agentOperationResume)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 0 || len(result.Entries) != 1 || result.Entries[0].Reason != "session operation is in progress" {
		t.Fatalf("concurrent operation was not preserved: %+v", result)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("concurrent operation's temp directory was removed: %v", err)
	}
}

func TestCancelledSeedSpawnRecordsTempExitForOrphanCollection(t *testing.T) {
	manager := New(Deps{Store: newFakeStore(), Workspace: &fakeWorkspace{}, TempRoot: privateSessionTempRoot(t), TempRetention: time.Hour})
	if _, err := manager.sessionTemps.Ensure(context.Background(), "session-cancelled"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	manager.rollbackSeedSpawnWorkspace(ctx, domain.SessionRecord{ID: "session-cancelled"}, ports.WorkspaceInfo{SessionID: "session-cancelled"}, nil, false, true)

	dir, found, err := manager.sessionTemps.Lookup(context.Background(), "session-cancelled")
	if err != nil || !found {
		t.Fatalf("cancelled spawn temp directory lookup found=%v err=%v", found, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".ao-session-temp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var owner sessiontemp.Owner
	if err := json.Unmarshal(data, &owner); err != nil {
		t.Fatal(err)
	}
	if owner.ExitedAt.IsZero() {
		t.Fatalf("cancelled spawn did not persist exit evidence: %+v", owner)
	}
}

func TestFailedSpawnRecordsTempExitAfterRuntimeDestroy(t *testing.T) {
	manager := New(Deps{
		Store: newFakeStore(), Workspace: &fakeWorkspace{}, Agents: testAgentRegistry{},
		TempRoot: privateSessionTempRoot(t), TempRetention: time.Hour,
	})
	if _, err := manager.sessionTemps.Ensure(context.Background(), "session-failed"); err != nil {
		t.Fatal(err)
	}
	manager.rollbackPreparedSpawnWorkspaceAfterFailure(
		context.Background(), domain.SessionRecord{ID: "session-failed"},
		ports.WorkspaceInfo{SessionID: "session-failed"}, nil, true,
	)
	dir, found, err := manager.sessionTemps.Lookup(context.Background(), "session-failed")
	if err != nil || !found {
		t.Fatalf("failed spawn temp directory lookup found=%v err=%v", found, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".ao-session-temp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var owner sessiontemp.Owner
	if err := json.Unmarshal(data, &owner); err != nil {
		t.Fatal(err)
	}
	if owner.ExitedAt.IsZero() {
		t.Fatalf("failed spawn did not persist exit evidence: %+v", owner)
	}
}

func TestUnknownRuntimeCreateFailureDoesNotAuthorizeTempGC(t *testing.T) {
	createdAt := time.Now().UTC()
	manager := New(Deps{
		Store: newFakeStore(), Workspace: &fakeWorkspace{}, TempRoot: privateSessionTempRoot(t), TempRetention: time.Hour,
		Clock: func() time.Time { return createdAt.Add(48 * time.Hour) },
	})
	if _, err := manager.sessionTemps.Ensure(context.Background(), "session-create-unknown"); err != nil {
		t.Fatal(err)
	}
	manager.rollbackSeedSpawnWorkspaceWithExitEvidence(
		context.Background(), domain.SessionRecord{ID: "session-create-unknown"},
		ports.WorkspaceInfo{SessionID: "session-create-unknown"}, nil, false, true, false,
	)

	dir, found, err := manager.sessionTemps.Lookup(context.Background(), "session-create-unknown")
	if err != nil || !found {
		t.Fatalf("unknown-create temp directory lookup found=%v err=%v", found, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".ao-session-temp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var owner sessiontemp.Owner
	if err := json.Unmarshal(data, &owner); err != nil {
		t.Fatal(err)
	}
	if !owner.ExitedAt.IsZero() {
		t.Fatalf("unknown runtime creation recorded process exit: %+v", owner)
	}

	result, err := manager.CollectSessionTemp(context.Background(), false, 8)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 0 || len(result.Entries) != 1 || result.Entries[0].Action != "skipped" {
		t.Fatalf("unknown runtime liveness was not preserved: %+v", result)
	}
}

func privateSessionTempRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ao-session-temp")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}
