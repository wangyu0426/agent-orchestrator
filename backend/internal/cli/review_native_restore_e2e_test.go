//go:build e2e && linux

package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// TestE2E_RealNativeReviewerIdleRestore deliberately DOES NOT trigger a PR
// review or call GitHub. It seeds one completed synthetic review using a
// previously captured native Codex session UUID, and calls only AO's idle
// reviewer restore endpoint. The temporary daemon's GH config is empty.
//
// This covers the previously observed failure: a dead direct PTY remained
// registered in the same daemon runtime; cleanup routed the bare reviewer
// name to tmux, so the replacement direct host rejected a duplicate name and
// hybridRuntime silently fell back to tmux.
//
// To run, explicitly provide AO_E2E_NATIVE_SESSION_ID from a disposable,
// already completed Codex reviewer trial. No default ID is hardcoded.
// Only the real Codex session metadata is reused; no GitHub review is posted.
func TestE2E_RealNativeReviewerIdleRestore(t *testing.T) {
	nativeID := strings.TrimSpace(os.Getenv("AO_E2E_NATIVE_SESSION_ID"))
	if nativeID == "" {
		t.Skip("requires AO_E2E_NATIVE_SESSION_ID from an already completed disposable Codex reviewer")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("real Codex CLI not installed")
	}
	e := newEnv(t)
	workspace := t.TempDir()
	if out, err := exec.Command("git", "init", "--quiet", workspace).CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated git repo: %v: %s", err, out)
	}

	ctx := context.Background()
	now := time.Now().UTC()
	const projectID = "native-restore-fixture"
	const reviewID = "native-restore-review"
	const runID = "native-restore-completed"
	const providerID = "LOCAL_ONLY_NO_GITHUB_REVIEW"
	const targetSHA = "e2e-completed-fixture"

	store, err := sqlitetest.Open(e.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: projectID, Kind: domain.ProjectKindScratch,
		Path: workspace, RegisteredAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: projectID, Kind: domain.KindWorker,
		Harness: domain.HarnessCodex, ReviewerHarness: domain.ReviewerCodex,
		Mode:         domain.SessionModeChat,
		HibernatedAt: &now,
		Activity:     domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		Metadata:     domain.SessionMetadata{WorkspacePath: workspace},
		CreatedAt:    now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertReview(ctx, domain.Review{
		ID: reviewID, SessionID: worker.ID, ProjectID: worker.ProjectID,
		Harness: domain.ReviewerCodex, InterfaceMode: domain.ReviewerInterfaceTUI,
		AgentSessionID: nativeID, ReviewerActivityState: domain.ActivityExited,
		PRURL:     "https://github.com/example/no-real-review/pull/1",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertReviewRun(ctx, domain.ReviewRun{
		ID: runID, ReviewID: reviewID, SessionID: worker.ID,
		Harness: domain.ReviewerCodex, PRURL: "https://github.com/example/no-real-review/pull/1",
		TargetSHA: targetSHA,
		Status:    domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
		Body:           "Synthetic completed review: no provider actions allowed.",
		GithubReviewID: providerID, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// The parent e2e harness drops AO_* and GH credentials, and allocates a
	// unique temporary run-file, data directory, and loopback port.
	// Only this child's reviewer profile is enabled; no auto-review is started.
	e.startDaemon(t, "AO_CODEX_REVIEW_LOOPBACK_PROFILE=1")
	t.Cleanup(func() {
		req, err := http.NewRequest(http.MethodPost,
			fmt.Sprintf("http://127.0.0.1:%d/api/v1/sessions/%s/reviews/kill", e.port, worker.ID),
			bytes.NewBufferString("{}"))
		if err != nil {
			t.Logf("scratch reviewer teardown request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 8 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Logf("scratch reviewer teardown: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			t.Errorf("scratch reviewer teardown HTTP %d: %s", resp.StatusCode, body)
		}
	})

	restore := func(label string) {
		t.Helper()
		addr := fmt.Sprintf("http://127.0.0.1:%d/api/v1/sessions/%s/reviews/restore", e.port, worker.ID)
		client := &http.Client{Timeout: 45 * time.Second}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr, bytes.NewBufferString("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s real AO restore API: %v", label, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16384))
		t.Logf("%s restore response HTTP %d: %s", label, resp.StatusCode, raw)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s restore HTTP %d", label, resp.StatusCode)
		}
	}

	type state struct {
		handle, agentID, generation, status, verdict, reviewID string
		runCount                                               int
	}
	readState := func() state {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+filepath.Join(e.dataDir, "ao.db")+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var v state
		if err := db.QueryRow(
			"SELECT reviewer_handle_id, agent_session_id, reviewer_launch_id FROM review WHERE session_id=? AND harness='codex' AND is_archived=0", worker.ID,
		).Scan(&v.handle, &v.agentID, &v.generation); err != nil {
			t.Fatalf("read persisted reviewer: %v", err)
		}
		if err := db.QueryRow(
			"SELECT count(*), min(status), min(verdict), min(github_review_id) FROM review_run WHERE session_id=?", worker.ID,
		).Scan(&v.runCount, &v.status, &v.verdict, &v.reviewID); err != nil {
			t.Fatalf("read persisted completed review: %v", err)
		}
		if v.runCount != 1 || v.status != "complete" ||
			v.verdict != "approved" || v.reviewID != providerID {
			t.Fatalf("review result changed: %+v", v)
		}
		if v.agentID != nativeID {
			t.Fatalf("native Codex session id changed: %q want %q", v.agentID, nativeID)
		}
		return v
	}
	type entry struct {
		SessionID  string `json:"sessionId"`
		PtyHostPID int    `json:"ptyHostPid"`
	}
	getHost := func() entry {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(e.runFile), "windows-pty-hosts.json"))
		if err != nil {
			t.Fatalf("isolated native PTY registry unavailable: %v", err)
		}
		var all []entry
		if err := json.Unmarshal(raw, &all); err != nil {
			t.Fatal(err)
		}
		target := "review-" + string(worker.ID)
		var matches []entry
		for _, x := range all {
			if x.SessionID == target {
				matches = append(matches, x)
			}
		}
		if len(matches) != 1 || matches[0].PtyHostPID <= 1 {
			t.Fatalf("expected one real direct PTY for %q, got %+v", target, matches)
		}
		return matches[0]
	}

	restore("initial idle native resume")
	first := readState()
	if !strings.HasPrefix(first.handle, "ptyhost-v1:") || first.generation == "" {
		t.Fatalf("initial Reviewer did not use direct PTY: %+v", first)
	}
	host1 := getHost()
	t.Logf("initial direct PTY: handle=%s hostPID=%d generation=%s", first.handle, host1.PtyHostPID, first.generation)

	// Identity fence: do not signal any PID unless it is exactly our test
	// binary executing pty-host for our one disposable worker.
	proc := fmt.Sprintf("/proc/%d", host1.PtyHostPID)
	exe, err := os.Readlink(filepath.Join(proc, "exe"))
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(filepath.Join(proc, "cmdline"))
	if err != nil {
		t.Fatal(err)
	}
	if host1.PtyHostPID == os.Getpid() || exe != aoBin ||
		!bytes.Contains(args, []byte("pty-host")) ||
		!bytes.Contains(args, []byte(host1.SessionID)) {
		t.Fatalf("refusing signal to unmatched PTY host pid=%d exe=%q argv=%q", host1.PtyHostPID, exe, args)
	}
	fd, err := unix.PidfdOpen(host1.PtyHostPID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	_ = unix.Close(fd)
	deadline := time.Now().Add(8 * time.Second)
	for {
		_, err := os.Stat(proc)
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scratch Reviewer PTY did not exit; refusing force-kill")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The daemon still has a stale direct-runtime reservation in its in-memory
	// map. The fix must destroy it using first.handle, not the bare name.
	restore("second idle native resume after dead PTY")
	second := readState()
	if second.handle != first.handle || !strings.HasPrefix(second.handle, "ptyhost-v1:") {
		t.Fatalf("native PTY was not preserved on restore: before=%q after=%q", first.handle, second.handle)
	}
	if second.generation == first.generation {
		t.Fatalf("reviewer launch generation unchanged: %q", second.generation)
	}
	host2 := getHost()
	if host2.PtyHostPID == host1.PtyHostPID {
		t.Fatalf("Reviewer PTY host was not replaced: %d", host2.PtyHostPID)
	}
	t.Logf("PASS: real Codex native ID retained, new direct PTY PID=%d, no new ReviewRun, zero GitHub credentials or API posts", host2.PtyHostPID)
}
