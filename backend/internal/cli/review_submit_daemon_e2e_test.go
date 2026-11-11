//go:build e2e

package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// TestE2E_ReviewSubmitDurableAcrossDaemonRestart exercises the real CLI,
// daemon HTTP controller, review service, and SQLite store without GitHub
// access or a synthetic verdict on any user's PR.
//
// It deliberately seeds an isolated, otherwise inactive review run before
// starting the daemon. Unlike the true Codex TUI reviewer acceptance test,
// this does NOT launch an agent or claim to have posted a GitHub review.
func TestE2E_ReviewSubmitDurableAcrossDaemonRestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const runID = "e2e-review-run"
	const bodyText = "Isolated review transport test: changes requested."
	const githubReviewID = "e2e-local-only-123"
	prURL := "https://github.com/example/isolated-review-test/pull/1"

	store, err := sqlitetest.Open(e.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "scratch-review", Kind: domain.ProjectKindScratch,
		Path: workspace, RegisteredAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "scratch-review", Kind: domain.KindWorker,
		Harness: domain.HarnessCodex, Mode: domain.SessionModeChat,
		HibernatedAt: &now,
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		Metadata: domain.SessionMetadata{WorkspacePath: workspace},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertReviewRun(ctx, domain.ReviewRun{
		ID: runID, SessionID: worker.ID, BatchID: "e2e-batch",
		Harness: domain.ReviewerCodex, PRURL: prURL,
		TargetSHA: "e2e-target-sha", Status: domain.ReviewRunRunning,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reviewBody := filepath.Join(t.TempDir(), "review.md")
	if err := os.WriteFile(reviewBody, []byte(bodyText), 0o600); err != nil {
		t.Fatal(err)
	}
	submit := []string{
		"review", "submit", string(worker.ID),
		"--run", runID, "--verdict", "changes_requested",
		"--body", reviewBody, "--review-id", githubReviewID,
	}

	e.startDaemon(t)
	for attempt := 0; attempt < 2; attempt++ {
		out, code := e.run(t, submit...)
		if code != 0 || !strings.Contains(out, "recorded changes_requested review") {
			t.Fatalf("submit %d: exit %d output %s", attempt+1, code, out)
		}
	}

	// An incompatible retry must be rejected rather than mutate a completed run.
	otherBody := filepath.Join(t.TempDir(), "conflicting-review.md")
	if err := os.WriteFile(otherBody, []byte("Different review body"), 0o600); err != nil {
		t.Fatal(err)
	}
	conflict := append([]string(nil), submit...)
	for i := 0; i < len(conflict)-1; i++ {
		if conflict[i] == "--body" {
			conflict[i+1] = otherBody
			break
		}
	}
	if out, code := e.run(t, conflict...); code == 0 {
		t.Fatalf("different review payload was accepted: %s", out)
	}

	if out, code := e.run(t, "stop"); code != 0 {
		t.Fatalf("stop scratch daemon: exit %d: %s", code, out)
	}

	// Reboot against the same temporary SQLite directory, then repeat the
	// exact result: it should be idempotent even after a process restart.
	e.startDaemon(t)
	if out, code := e.run(t, submit...); code != 0 ||
		!strings.Contains(out, "recorded changes_requested review") {
		t.Fatalf("resubmit after restart: exit %d: %s", code, out)
	}
	if out, code := e.run(t, "stop"); code != 0 {
		t.Fatalf("second stop scratch daemon: exit %d: %s", code, out)
	}

	reopened, err := sqlitetest.Open(e.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, ok, err := reopened.GetReviewRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("persisted review: ok=%v err=%v", ok, err)
	}
	if got.SessionID != worker.ID || got.PRURL != prURL ||
		got.TargetSHA != "e2e-target-sha" ||
		got.Status != domain.ReviewRunComplete ||
		got.Verdict != domain.VerdictChangesRequested ||
		got.Body != bodyText || got.GithubReviewID != githubReviewID {
		t.Fatalf("persisted review differs from submitted result: %+v", got)
	}
}
