package reviewgateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func testManifest(t *testing.T) Manifest {
	t.Helper()
	root := t.TempDir()
	promptRoot := filepath.Join(root, "prompts")
	if err := os.Mkdir(promptRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	return Manifest{
		ReviewerID:      "reviewer-1",
		ReviewSessionID: "review-session-1",
		WorkerSessionID: "worker-1",
		WorkspacePath:   filepath.Join(root, "source"),
		TaskPromptRoot:  promptRoot,
		Tasks: []Task{{
			RunID: "run-1", PRURL: "https://github.com/acme/widgets/pull/42",
			TargetSHA:      "0123456789abcdef",
			TaskPromptFile: filepath.Join(promptRoot, "run-1.md"),
		}},
	}
}

func TestPrepareEnvironmentUsesPrivateAOOwnedDirectoriesAndImmutableManifest(t *testing.T) {
	dataDir := t.TempDir()
	manifest := testManifest(t)
	env, err := PrepareEnvironment(dataDir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{env.Root, env.WorkingDirectory, env.ConfigRoot, env.StateRoot, env.CacheRoot, env.TempRoot} {
		rel, err := filepath.Rel(dataDir, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Fatalf("path %q is outside data dir %q", path, dataDir)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %q mode = %v, err = %v", path, info.Mode().Perm(), err)
		}
	}
	if got := env.TUIEnvironment(); got["HOME"] != env.ConfigRoot || got["TMPDIR"] != env.TempRoot || got["XDG_CACHE_HOME"] != env.CacheRoot {
		t.Fatalf("TUI environment = %#v", got)
	}
	info, err := os.Stat(env.ManifestPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %v, err = %v", info.Mode().Perm(), err)
	}

	again, err := PrepareEnvironment(dataDir, manifest)
	if err != nil || again.ManifestPath != env.ManifestPath {
		t.Fatalf("idempotent prepare = %+v, %v", again, err)
	}
	manifest.Tasks[0].TargetSHA = "1111111111111111"
	changed, err := PrepareEnvironment(dataDir, manifest)
	if err != nil || changed.ManifestPath == env.ManifestPath {
		t.Fatalf("changed authority must create a new immutable manifest: %+v, %v", changed, err)
	}
}

func TestPrepareHostTrustedEnvironmentUsesPrivateAOOwnedDirectories(t *testing.T) {
	dataDir := t.TempDir()
	env, err := PrepareHostTrustedEnvironment(dataDir, "review-worker-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{env.Root, env.WorkingDirectory, env.ConfigRoot, env.StateRoot, env.CacheRoot, env.TempRoot} {
		rel, relErr := filepath.Rel(dataDir, path)
		info, statErr := os.Stat(path)
		if relErr != nil || strings.HasPrefix(rel, "..") || statErr != nil {
			t.Fatalf("private AO-owned directory %q: rel=%q relErr=%v statErr=%v", path, rel, relErr, statErr)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("private AO-owned directory %q mode = %v", path, info.Mode().Perm())
		}
	}
	got := env.TUIEnvironment()
	if got["HOME"] != env.ConfigRoot || got["XDG_STATE_HOME"] != env.StateRoot || got["TMPDIR"] != env.TempRoot {
		t.Fatalf("TUI environment = %#v", got)
	}
	if _, err := PrepareHostTrustedEnvironment("relative", "review-worker-1"); err == nil {
		t.Fatal("relative AO data directory accepted")
	}
	if _, err := PrepareHostTrustedEnvironment(dataDir, "../escape"); err == nil {
		t.Fatal("unsafe reviewer id accepted")
	}
}

func TestPrepareEnvironmentRejectsTraversalAndInvalidAuthorization(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"reviewer traversal", func(m *Manifest) { m.ReviewerID = "../escape" }},
		{"duplicate run", func(m *Manifest) { m.Tasks = append(m.Tasks, m.Tasks[0]) }},
		{"ref injection", func(m *Manifest) { m.Tasks[0].TargetSHA = "--help" }},
		{"non github PR", func(m *Manifest) { m.Tasks[0].PRURL = "https://evil.invalid/acme/widgets/pull/42" }},
		{"prompt escape", func(m *Manifest) { m.Tasks[0].TaskPromptFile = filepath.Join(m.TaskPromptRoot, "..", "escape") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := testManifest(t)
			tt.mutate(&manifest)
			if _, err := PrepareEnvironment(t.TempDir(), manifest); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestPrepareEnvironmentRequiresAbsoluteDataDir(t *testing.T) {
	if _, err := PrepareEnvironment("relative", testManifest(t)); err == nil {
		t.Fatal("relative AO data directory was accepted")
	}
}

func TestResultInboxDelayedDeliveryAndDuplicateResult(t *testing.T) {
	dataDir := t.TempDir()
	manifest := testManifest(t)
	if _, _, err := PrepareActiveEnvironment(dataDir, manifest); err != nil {
		t.Fatal(err)
	}
	inbox, err := NewResultInbox(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	submission := Submission{RunID: "run-1", Verdict: "approved", Body: "reviewed", GithubReviewID: "5841980481"}
	first, err := inbox.QueueResult(manifest, submission)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := inbox.QueueResult(manifest, submission)
	if err != nil {
		t.Fatalf("identical duplicate: %v", err)
	}
	if duplicate.ResultHash != first.ResultHash {
		t.Fatalf("duplicate hash = %s, want %s", duplicate.ResultHash, first.ResultHash)
	}
	delayedCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := inbox.WaitForReceipt(delayedCtx, first, 5*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("receipt wait error = %v, want deadline exceeded while host delivery is delayed", err)
	}
	pending, err := inbox.Pending(context.Background())
	if err != nil || len(pending) != 1 || pending[0].Result.GithubReviewID != "5841980481" {
		t.Fatalf("pending = %+v, err=%v", pending, err)
	}
	if err := inbox.Acknowledge(first, "delivered", ""); err != nil {
		t.Fatal(err)
	}
	receipt, err := inbox.WaitForReceipt(context.Background(), first, time.Millisecond)
	if err != nil || receipt.Status != "delivered" {
		t.Fatalf("receipt = %+v, err=%v", receipt, err)
	}
	// A submit retry after acknowledgement reuses the receipt and is safe for
	// the host to process again without making a second state transition.
	if _, err := inbox.QueueResult(manifest, submission); err != nil {
		t.Fatalf("retry after acknowledgement: %v", err)
	}
	items, err := inbox.Pending(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("replayed item = %+v, err=%v", items, err)
	}
	if err := inbox.Acknowledge(items[0], "delivered", ""); err != nil {
		t.Fatal(err)
	}
	items, err = inbox.Pending(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("pending after duplicate acknowledgement = %+v, err=%v", items, err)
	}
}

func TestResultInboxRejectsChangedDuplicateAndChecksExactRunIdentity(t *testing.T) {
	dataDir := t.TempDir()
	manifest := testManifest(t)
	inbox, err := NewResultInbox(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	item, err := inbox.QueueResult(manifest, Submission{RunID: "run-1", Verdict: "approved", GithubReviewID: "5841980481"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.QueueResult(manifest, Submission{RunID: "run-1", Verdict: "changes_requested", Body: "new verdict", GithubReviewID: "5841980481"}); err == nil {
		t.Fatal("different result replaced a pending result")
	}
	run := domain.ReviewRun{
		ID: "run-1", ReviewID: manifest.ReviewSessionID, SessionID: manifest.WorkerSessionID,
		PRURL: manifest.Tasks[0].PRURL, TargetSHA: manifest.Tasks[0].TargetSHA, Status: domain.ReviewRunRunning,
	}
	if pending, err := inbox.HasPendingResults(context.Background(), manifest.ReviewerID, []domain.ReviewRun{run}); err != nil || !pending {
		t.Fatalf("exact pending identity = %v, err=%v", pending, err)
	}
	run.TargetSHA = "fedcba9876543210"
	if pending, err := inbox.HasPendingResults(context.Background(), manifest.ReviewerID, []domain.ReviewRun{run}); err != nil || pending {
		t.Fatalf("stale-head pending identity = %v, err=%v, want false", pending, err)
	}
	if err := inbox.Acknowledge(item, "rejected", "test cleanup"); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareActiveEnvironmentPublishesSessionBoundManifest(t *testing.T) {
	dataDir := t.TempDir()
	manifest := testManifest(t)
	env, pointer, err := PrepareActiveEnvironment(dataDir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewerID != manifest.ReviewerID || got.ReviewSessionID != manifest.ReviewSessionID || got.WorkerSessionID != manifest.WorkerSessionID || filepath.Dir(pointer) != env.Root {
		t.Fatalf("active manifest = %+v, pointer = %q", got, pointer)
	}
}

func TestHasPendingResultsWaitsForRecentPartialWrite(t *testing.T) {
	dataDir := t.TempDir()
	manifest := testManifest(t)
	env, err := PrepareResultEnvironment(dataDir, manifest.ReviewerID)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := NewResultInbox(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	run := domain.ReviewRun{
		ID: "run-1", ReviewID: manifest.ReviewSessionID, SessionID: manifest.WorkerSessionID,
		PRURL: manifest.Tasks[0].PRURL, TargetSHA: manifest.Tasks[0].TargetSHA, Status: domain.ReviewRunRunning,
	}
	path := filepath.Join(env.ResultPendingDir, "run-1.json")
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if pending, err := inbox.HasPendingResults(context.Background(), manifest.ReviewerID, []domain.ReviewRun{run}); err != nil || !pending {
		t.Fatalf("recent partial result = %v, err=%v, want pending", pending, err)
	}
	old := time.Now().Add(-3 * time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if pending, err := inbox.HasPendingResults(context.Background(), manifest.ReviewerID, []domain.ReviewRun{run}); err != nil || pending {
		t.Fatalf("stable malformed result = %v, err=%v, want not pending", pending, err)
	}
}
