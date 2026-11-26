package sessiontemp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnsureCreatesPrivateSessionDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ao", "tmp")
	store := New(root, time.Hour)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	first, err := store.Ensure(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.Ensure(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Ensure(context.Background(), "session-2")
	if err != nil {
		t.Fatal(err)
	}
	if first != again || first == second {
		t.Fatalf("session paths = %q, %q, %q", first, again, second)
	}
	for path, want := range map[string]os.FileMode{first: 0o700, filepath.Join(first, manifestName): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s permissions = %o, want %o", path, got, want)
		}
	}
	owner, err := readOwner(first)
	if err != nil {
		t.Fatal(err)
	}
	if owner.SessionID != "session-1" || !owner.CreatedAt.Equal(now) {
		t.Fatalf("owner = %+v", owner)
	}
	for _, id := range []string{"", "../outside", "a/b", `a\b`, ".", ".."} {
		if _, err := store.Ensure(context.Background(), id); err == nil {
			t.Errorf("Ensure(%q) unexpectedly succeeded", id)
		}
	}
}

func TestCollectRecoversExitedOwnershipAfterRestart(t *testing.T) {
	root := privateTempRoot(t)
	old := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	now := old.Add(48 * time.Hour)
	first := New(root, time.Hour)
	first.now = func() time.Time { return old }
	dir, err := first.Ensure(context.Background(), "session-success")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build.out"), []byte("temporary data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.MarkExited(context.Background(), "session-success"); err != nil {
		t.Fatal(err)
	}
	manifestInfo, err := os.Stat(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := int64(len("temporary data")) + manifestInfo.Size()

	// A fresh Store models daemon restart: ownership and exit evidence live on disk.
	restarted := New(root, time.Hour)
	result, err := restarted.Collect(context.Background(), now, 8, false, func(_ context.Context, owner Owner) (Eligibility, error) {
		if owner.SessionID != "session-success" || owner.ExitedAt.IsZero() {
			t.Fatalf("restart did not recover owner metadata: %+v", owner)
		}
		return Eligibility{Collect: true, ExitedAt: owner.ExitedAt}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || result.ReclaimedBytes != wantBytes {
		t.Fatalf("collection result = %+v", result)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collected directory still exists or stat failed: %v", err)
	}
}

func TestCollectFailsClosedForUnknownLiveness(t *testing.T) {
	store, dir := agedStore(t, "session-unknown")
	result, err := store.Collect(context.Background(), time.Now().UTC(), 8, false, func(context.Context, Owner) (Eligibility, error) {
		return Eligibility{Reason: "runtime probe is unknown"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 0 || len(result.Entries) != 1 || result.Entries[0].Action != "skipped" {
		t.Fatalf("unknown liveness result = %+v", result)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("unknown-liveness directory was removed: %v", err)
	}
}

func TestCollectRetentionBeginsAtConfirmedExit(t *testing.T) {
	store := New(privateTempRoot(t), time.Hour)
	confirmedAt := time.Now().UTC()
	store.now = func() time.Time { return confirmedAt }
	dir, err := store.Ensure(context.Background(), "session-late-exit")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := readOwner(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner.CreatedAt = confirmedAt.Add(-30 * 24 * time.Hour)
	if err := writeOwner(dir, owner); err != nil {
		t.Fatal(err)
	}
	callback := func(context.Context, Owner) (Eligibility, error) {
		return Eligibility{Collect: true}, nil
	}

	preview, err := store.Collect(context.Background(), confirmedAt, 8, true, callback)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Entries) != 1 || preview.Entries[0].Action != "skipped" || !strings.Contains(preview.Entries[0].Reason, "retention starts") {
		t.Fatalf("dry-run result = %+v", preview)
	}
	owner, err = readOwner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !owner.ExitedAt.IsZero() {
		t.Fatalf("dry-run persisted exit evidence: %+v", owner)
	}

	firstPass, err := store.Collect(context.Background(), confirmedAt, 8, false, callback)
	if err != nil {
		t.Fatal(err)
	}
	if firstPass.Removed != 0 {
		t.Fatalf("first pass removed data before retention: %+v", firstPass)
	}
	owner, err = readOwner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !owner.ExitedAt.Equal(confirmedAt) {
		t.Fatalf("persisted exit time = %s, want %s", owner.ExitedAt, confirmedAt)
	}
	secondPass, err := store.Collect(context.Background(), confirmedAt.Add(time.Hour), 8, false, callback)
	if err != nil {
		t.Fatal(err)
	}
	if secondPass.Removed != 1 {
		t.Fatalf("eligible post-retention directory not removed: %+v", secondPass)
	}
}

func TestCollectRejectsSymlinksAndUnownedPaths(t *testing.T) {
	store, dir := agedStore(t, "session-links")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Mkdir(filepath.Join(store.root, "session-unowned"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := store.Collect(context.Background(), time.Now().UTC(), 8, false, func(context.Context, Owner) (Eligibility, error) {
		return Eligibility{Collect: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 0 || len(result.Entries) != 1 || result.Entries[0].Action != "skipped" {
		t.Fatalf("unsafe directory result = %+v", result)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatalf("outside target changed: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(store.root, "session-unowned")); err != nil {
		t.Fatalf("unowned directory was removed: %v", err)
	}
}

func TestEnsureRejectsSymlinkRoot(t *testing.T) {
	actual := t.TempDir()
	link := filepath.Join(t.TempDir(), "tmp-link")
	if err := os.Symlink(actual, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := New(link, time.Hour).Ensure(context.Background(), "session-1"); err == nil {
		t.Fatal("Ensure accepted a symlink root")
	}
	entries, err := os.ReadDir(actual)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink target was modified: %v", entries)
	}
}

func TestEnsureRejectsGlobalTempRootWithoutChangingPermissions(t *testing.T) {
	root := os.TempDir()
	before, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(root, time.Hour).Ensure(context.Background(), "session-1"); err == nil {
		t.Fatal("Ensure accepted the host's global temp root")
	}
	after, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != after.Mode().Perm() {
		t.Fatalf("global temp root permissions changed from %o to %o", before.Mode().Perm(), after.Mode().Perm())
	}
}

func TestEnsureRejectsNonPrivateExistingRootWithoutChangingPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared-temp")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root, time.Hour).Ensure(context.Background(), "session-1"); err == nil {
		t.Fatal("Ensure accepted a non-private existing root")
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("existing root permissions changed to %o, want 755", got)
	}
}

func TestCollectReportsCleanupFailure(t *testing.T) {
	store, dir := agedStore(t, "session-cleanup-failure")
	store.removeAll = func(string) error { return errors.New("disk error") }
	result, err := store.Collect(context.Background(), time.Now().UTC(), 8, false, func(context.Context, Owner) (Eligibility, error) {
		return Eligibility{Collect: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Action != "failed" || !strings.Contains(result.Entries[0].Error, "disk error") {
		t.Fatalf("cleanup failure result = %+v", result)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("cleanup failure removed directory: %v", err)
	}
}

func TestCollectSerializesConcurrentSpawnWithGC(t *testing.T) {
	store, _ := agedStore(t, "session-old")
	entered := make(chan struct{})
	release := make(chan struct{})
	gcDone := make(chan error, 1)
	go func() {
		_, err := store.Collect(context.Background(), time.Now().UTC(), 8, false, func(context.Context, Owner) (Eligibility, error) {
			close(entered)
			<-release
			return Eligibility{Reason: "session is active"}, nil
		})
		gcDone <- err
	}()
	<-entered
	spawnDone := make(chan error, 1)
	go func() {
		_, err := store.Ensure(context.Background(), "session-new")
		spawnDone <- err
	}()
	select {
	case err := <-spawnDone:
		t.Fatalf("concurrent spawn bypassed the root lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-gcDone; err != nil {
		t.Fatal(err)
	}
	if err := <-spawnDone; err != nil {
		t.Fatal(err)
	}
}

func TestCollectCompletesBoundedSweepsAndStartsNextSweep(t *testing.T) {
	store := New(privateTempRoot(t), time.Hour)
	for i := 0; i < 5; i++ {
		if err := os.Mkdir(filepath.Join(store.root, fmt.Sprintf("session-unowned-%02d", i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var scanned int
	var result Result
	for pass := 0; pass < 10; pass++ {
		var err error
		result, err = store.Collect(context.Background(), time.Now().UTC(), 2, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		scanned += result.Scanned
		if !result.More {
			break
		}
	}
	if result.More || scanned < 5 {
		t.Fatalf("bounded sweep did not finish: last=%+v scanned=%d", result, scanned)
	}
	result, err := store.Collect(context.Background(), time.Now().UTC(), 2, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.More {
		t.Fatalf("new sweep did not report its remaining entries: %+v", result)
	}
}

func agedStore(t *testing.T, sessionID string) (*Store, string) {
	t.Helper()
	now := time.Now().UTC()
	store := New(privateTempRoot(t), time.Hour)
	store.now = func() time.Time { return now.Add(-48 * time.Hour) }
	dir, err := store.Ensure(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := readOwner(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner.ExitedAt = now.Add(-2 * time.Hour)
	if err := writeOwner(dir, owner); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }
	return store, dir
}

func privateTempRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ao-session-temp")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}
