package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// TestWaitForStoppedKeepsRunFileFromConcurrentStart guards against deleting a
// fresh daemon's handshake: if a concurrent `ao start` replaces running.json
// with a new live PID while we are polling the PID we stopped, waitForStopped
// must report stopped but leave the new run-file intact.
func TestWaitForStoppedKeepsRunFileFromConcurrentStart(t *testing.T) {
	dir := t.TempDir()
	runFile := filepath.Join(dir, "running.json")

	const stoppedPID, newPID = 1111, 2222
	// running.json now belongs to a different, live daemon.
	if err := runfile.Write(runFile, runfile.Info{PID: newPID, Port: 3001, StartedAt: time.Unix(100, 0).UTC()}); err != nil {
		t.Fatal(err)
	}

	c := &commandContext{deps: Deps{
		ProcessAlive: func(pid int) bool { return pid == newPID }, // stoppedPID is dead
		Now:          func() time.Time { return time.Unix(200, 0).UTC() },
		Sleep:        func(time.Duration) {},
	}.withDefaults()}

	st, err := c.waitForStopped(context.Background(), stoppedPID, runFile, dir, time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != stateStopped {
		t.Fatalf("state = %q, want stopped", st.State)
	}

	info, err := runfile.Read(runFile)
	if err != nil {
		t.Fatal(err)
	}
	if info == nil {
		t.Fatal("new daemon's run-file was deleted by stop of a different PID")
		return
	}
	if info.PID != newPID {
		t.Fatalf("run-file PID = %d, want %d (new daemon)", info.PID, newPID)
	}
}

// TestWaitForStoppedRemovesOwnRunFile confirms the normal path still cleans up:
// when the dead PID owns the run-file, it is removed.
func TestWaitForStoppedRemovesOwnRunFile(t *testing.T) {
	dir := t.TempDir()
	runFile := filepath.Join(dir, "running.json")

	const stoppedPID = 1111
	if err := runfile.Write(runFile, runfile.Info{PID: stoppedPID, Port: 3001, StartedAt: time.Unix(100, 0).UTC()}); err != nil {
		t.Fatal(err)
	}

	c := &commandContext{deps: Deps{
		ProcessAlive: func(int) bool { return false },
		Now:          func() time.Time { return time.Unix(200, 0).UTC() },
		Sleep:        func(time.Duration) {},
	}.withDefaults()}

	st, err := c.waitForStopped(context.Background(), stoppedPID, runFile, dir, time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != stateStopped {
		t.Fatalf("state = %q, want stopped", st.State)
	}
	info, err := runfile.Read(runFile)
	if err != nil {
		t.Fatal(err)
	}
	if info != nil {
		t.Fatalf("own run-file should have been removed, got %#v", info)
	}
}

func TestWaitForStoppedWaitsAfterRunFileRemovedUntilProcessExits(t *testing.T) {
	dir := t.TempDir()
	runFile := filepath.Join(dir, "running.json")

	const stoppedPID = 1111
	now := time.Unix(200, 0).UTC()
	aliveChecks := 0
	sleeps := 0
	c := &commandContext{deps: Deps{
		ProcessAlive: func(int) bool {
			aliveChecks++
			return aliveChecks < 3
		},
		Now: func() time.Time {
			return now
		},
		Sleep: func(d time.Duration) {
			sleeps++
			now = now.Add(d)
		},
	}.withDefaults()}

	st, err := c.waitForStopped(context.Background(), stoppedPID, runFile, dir, time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != stateStopped {
		t.Fatalf("state = %q, want stopped", st.State)
	}
	if sleeps == 0 {
		t.Fatal("waitForStopped returned before waiting for process exit")
	}
	if aliveChecks < 3 {
		t.Fatalf("process checks = %d, want at least 3", aliveChecks)
	}
}

// TestWaitForStoppedReportsStoppedWhenRunFileGoneButProcessLingers covers
// issue #2214: once the daemon has removed its run-file (its liveness marker)
// the stop is committed, so if the process is still draining background workers
// past the timeout, waitForStopped must report stopped rather than erroring.
func TestWaitForStoppedReportsStoppedWhenRunFileGoneButProcessLingers(t *testing.T) {
	dir := t.TempDir()
	runFile := filepath.Join(dir, "running.json") // never written: run-file already gone

	const stoppedPID = 1111
	now := time.Unix(200, 0).UTC()
	c := &commandContext{deps: Deps{
		ProcessAlive: func(int) bool { return true }, // process never exits
		Now:          func() time.Time { return now },
		Sleep:        func(d time.Duration) { now = now.Add(d) },
	}.withDefaults()}

	st, err := c.waitForStopped(context.Background(), stoppedPID, runFile, dir, time.Second, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.State != stateStopped {
		t.Fatalf("state = %q, want stopped", st.State)
	}
}


// An isolated worker must not treat its inability to see the host PID as
// evidence the daemon has finished shutdown while the run-file remains.
func TestWaitForStoppedKeepsInvisibleLiveHostRunFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "running.json")
	const hostPID = 424242
	if err := runfile.Write(path, runfile.Info{PID: hostPID, Port: 3301, StartedAt: time.Unix(100, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(200, 0).UTC()
	// Simulate a successful shutdown ACK while the daemon has not removed
	// its run-file. This must not be mistaken for confirmed process exit.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shutdown" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := &commandContext{deps: (Deps{
		HTTPClient: srv.Client(),
		ProcessAlive: func(int) bool { return false },
		Now: func() time.Time { return now },
		Sleep: func(d time.Duration) { now = now.Add(d) },
	}).withDefaults()}
	if err := c.requestShutdown(context.Background(), serverPort(t, srv.URL)); err != nil {
		t.Fatal(err)
	}
	st, err := c.waitForStopped(context.Background(), hostPID, path, dir, time.Second, false)
	if err == nil || st.State == stateStopped {
		t.Fatalf("wait = %+v, %v; must not report stopped while owner cannot be verified", st, err)
	}
	info, err := runfile.Read(path)
	if err != nil { t.Fatal(err) }
	if info == nil || info.PID != hostPID {
		t.Fatalf("live host run-file must remain intact, got %#v", info)
	}
}
