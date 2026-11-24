// Package sessiontemp owns private temporary directories for AO sessions.
// Directories are discoverable only through their versioned ownership manifest;
// collection never sweeps the host's shared temporary directory.
package sessiontemp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/datadirlock"
)

const (
	manifestName      = ".ao-session-temp.json"
	manifestVersion   = 1
	DefaultBatchLimit = 64
	maxBatchLimit     = 256
	DefaultRetention  = 7 * 24 * time.Hour
)

// Owner is the durable identity and age of one session temp directory.
type Owner struct {
	Version   int       `json:"version"`
	SessionID string    `json:"sessionId"`
	CreatedAt time.Time `json:"createdAt"`
	ExitedAt  time.Time `json:"exitedAt,omitempty"`
}

// Eligibility is a conservative lifecycle decision made by the session
// manager, which has access to durable session records and runtime probes.
type Eligibility struct {
	Collect  bool
	Reason   string
	ExitedAt time.Time
}

// EntryResult describes one owned directory considered by a collection pass.
type EntryResult struct {
	SessionID string `json:"sessionId,omitempty"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	Bytes     int64  `json:"bytes"`
	Error     string `json:"error,omitempty"`
}

// Result is a bounded diagnostics snapshot. ReclaimedBytes is populated only
// after a successful removal; dry runs report the same estimate in entries.
type Result struct {
	Root           string        `json:"root"`
	Scanned        int           `json:"scanned"`
	Removed        int           `json:"removed"`
	ReclaimedBytes int64         `json:"reclaimedBytes"`
	More           bool          `json:"more"`
	DryRun         bool          `json:"dryRun"`
	Entries        []EntryResult `json:"entries"`
}

// Store serializes create, mark-exited, and GC operations in-process and across
// daemon processes that share a configured root.
type Store struct {
	root      string
	retention time.Duration
	now       func() time.Time
	removeAll func(string) error
	mu        sync.Mutex
	cursor    string
	cycleEnd  string
}

// New constructs a store. Root creation is lazy so malformed or unwritable
// temp configuration does not prevent the daemon from serving existing data.
func New(root string, retention time.Duration) *Store {
	if retention <= 0 {
		retention = DefaultRetention
	}
	return &Store{root: strings.TrimSpace(root), retention: retention, now: func() time.Time { return time.Now().UTC() }, removeAll: os.RemoveAll}
}

// Root returns the resolved AO-owned root after its first successful use.
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Ensure creates or returns the private temp directory for sessionID.
func (s *Store) Ensure(ctx context.Context, sessionID string) (string, error) {
	if s == nil {
		return "", errors.New("session temp store is disabled")
	}
	if err := validateSessionID(sessionID); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, unlock, err := s.lockedRoot(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	dir := filepath.Join(root, sessionDirName(sessionID))
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("session temp path is not a real directory: %s", dir)
		}
		owner, err := readOwner(dir)
		if err != nil {
			return "", err
		}
		if owner.SessionID != sessionID {
			return "", fmt.Errorf("session temp ownership mismatch for %s", sessionID)
		}
		owner.ExitedAt = time.Time{}
		if err := writeOwner(dir, owner); err != nil {
			return "", fmt.Errorf("refresh session temp ownership: %w", err)
		}
		return dir, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("inspect session temp path: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("create session temp directory: %w", err)
	}
	owner := Owner{Version: manifestVersion, SessionID: sessionID, CreatedAt: s.now()}
	if err := writeOwner(dir, owner); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("write session temp ownership: %w", err)
	}
	return dir, nil
}

// Lookup returns the existing private directory without creating or refreshing
// it. Teardown subprocesses use this so cleanup does not recreate storage for a
// session whose temp directory was already collected or whose spawn never ran.
func (s *Store) Lookup(ctx context.Context, sessionID string) (string, bool, error) {
	if s == nil {
		return "", false, nil
	}
	if err := validateSessionID(sessionID); err != nil {
		return "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, unlock, err := s.lockedRoot(ctx)
	if err != nil {
		return "", false, err
	}
	defer unlock()
	dir := filepath.Join(root, sessionDirName(sessionID))
	owner, err := readOwner(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if owner.Version != manifestVersion || owner.SessionID != sessionID {
		return "", false, fmt.Errorf("session temp ownership mismatch for %s", sessionID)
	}
	return dir, true, nil
}

// MarkExited records that the caller has conclusive evidence that the session
// process tree or controller has exited. It does not make an active session
// collectible by itself; GC still checks durable session state when available.
func (s *Store) MarkExited(ctx context.Context, sessionID string) error {
	if s == nil {
		return nil
	}
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, unlock, err := s.lockedRoot(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	dir := filepath.Join(root, sessionDirName(sessionID))
	owner, err := readOwner(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if owner.SessionID != sessionID {
		return fmt.Errorf("session temp ownership mismatch for %s", sessionID)
	}
	owner.ExitedAt = s.now()
	if err := writeOwner(dir, owner); err != nil {
		return fmt.Errorf("record session temp process exit: %w", err)
	}
	return nil
}

// Collect removes at most limit owned directories old enough for retention.
// The eligibility callback must return Collect=true only on durable terminal
// state and confirmed process/controller exit. Errors and unknown states fail
// closed. No directory with a symlink in its tree is removed.
func (s *Store) Collect(
	ctx context.Context,
	now time.Time,
	limit int,
	dryRun bool,
	eligible func(context.Context, Owner) (Eligibility, error),
) (Result, error) {
	if s == nil {
		return Result{}, errors.New("session temp store is disabled")
	}
	if limit <= 0 {
		limit = DefaultBatchLimit
	}
	if limit > maxBatchLimit {
		limit = maxBatchLimit
	}
	if now.IsZero() {
		now = s.now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, unlock, err := s.lockedRoot(ctx)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	result := Result{Root: root, DryRun: dryRun, Entries: []EntryResult{}}
	entries, err := os.ReadDir(root)
	if err != nil {
		return result, fmt.Errorf("read session temp root: %w", err)
	}
	if len(entries) == 0 {
		if !dryRun {
			s.cursor, s.cycleEnd = "", ""
		}
		return result, nil
	}
	cycleEnd := s.cycleEnd
	if cycleEnd == "" {
		cycleEnd = entries[len(entries)-1].Name()
	}
	cycleEndIndex := sort.Search(len(entries), func(i int) bool { return entries[i].Name() >= cycleEnd })
	if cycleEndIndex == len(entries) {
		// The old boundary disappeared between passes. Start a fresh sweep over
		// the remaining entries so a stale cursor cannot skip them indefinitely.
		cycleEnd = entries[len(entries)-1].Name()
		cycleEndIndex = len(entries) - 1
	}
	start := 0
	if s.cursor != "" {
		start = sort.Search(len(entries), func(i int) bool { return entries[i].Name() > s.cursor })
		if start == len(entries) || start > cycleEndIndex {
			start = 0
		}
	}
	end := start + limit
	if end < start || end > cycleEndIndex+1 {
		end = cycleEndIndex + 1
	}
	selected := append([]os.DirEntry(nil), entries[start:end]...)
	result.More = len(selected) > 0 && selected[len(selected)-1].Name() < cycleEnd
	if !dryRun {
		if result.More {
			s.cursor = selected[len(selected)-1].Name()
			s.cycleEnd = cycleEnd
		} else {
			s.cursor, s.cycleEnd = "", ""
		}
	}
	for _, entry := range selected {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Scanned++
		name := entry.Name()
		if !strings.HasPrefix(name, "session-") {
			continue
		}
		dir := filepath.Join(root, name)
		if err := containedPath(root, dir); err != nil {
			result.Entries = append(result.Entries, EntryResult{Action: "skipped", Reason: "path outside owned root"})
			continue
		}
		info, err := os.Lstat(dir)
		if err != nil {
			result.Entries = append(result.Entries, EntryResult{Action: "failed", Reason: "inspect directory", Error: err.Error()})
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		owner, err := readOwner(dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				result.Entries = append(result.Entries, EntryResult{Action: "skipped", Reason: "ownership metadata is invalid", Error: err.Error()})
			}
			continue
		}
		if owner.Version != manifestVersion || name != sessionDirName(owner.SessionID) || owner.CreatedAt.IsZero() {
			result.Entries = append(result.Entries, EntryResult{SessionID: owner.SessionID, Action: "skipped", Reason: "ownership metadata does not match directory"})
			continue
		}
		if owner.ExitedAt.IsZero() && now.Sub(owner.CreatedAt) < s.retention || !owner.ExitedAt.IsZero() && now.Sub(owner.ExitedAt) < s.retention {
			result.Entries = append(result.Entries, EntryResult{SessionID: owner.SessionID, Action: "skipped", Reason: "retention period has not elapsed"})
			continue
		}
		decision := Eligibility{Reason: "liveness or session state is unknown"}
		if eligible != nil {
			decision, err = eligible(ctx, owner)
		}
		if err != nil {
			result.Entries = append(result.Entries, EntryResult{SessionID: owner.SessionID, Action: "skipped", Reason: "lifecycle check failed", Error: err.Error()})
			continue
		}
		if !decision.Collect {
			if decision.Reason == "" {
				decision.Reason = "liveness or session state is unknown"
			}
			result.Entries = append(result.Entries, EntryResult{SessionID: owner.SessionID, Action: "skipped", Reason: decision.Reason})
			continue
		}
		if owner.ExitedAt.IsZero() {
			owner.ExitedAt = decision.ExitedAt
			if owner.ExitedAt.IsZero() {
				owner.ExitedAt = now
			}
			owner.ExitedAt = owner.ExitedAt.UTC()
			if !dryRun {
				if err := writeOwner(dir, owner); err != nil {
					result.Entries = append(result.Entries, EntryResult{SessionID: owner.SessionID, Action: "failed", Reason: "persist exit evidence", Error: err.Error()})
					continue
				}
			}
			if now.Sub(owner.ExitedAt) < s.retention {
				result.Entries = append(result.Entries, EntryResult{SessionID: owner.SessionID, Action: "skipped", Reason: "retention starts after confirmed process exit"})
				continue
			}
		}
		bytes, err := treeSize(ctx, dir)
		if err != nil {
			result.Entries = append(result.Entries, EntryResult{SessionID: owner.SessionID, Action: "skipped", Reason: "directory contains unsafe links or cannot be measured", Error: err.Error()})
			continue
		}
		if dryRun {
			result.Entries = append(result.Entries, EntryResult{SessionID: owner.SessionID, Action: "would_remove", Bytes: bytes})
			continue
		}
		if err := s.removeAll(dir); err != nil {
			result.Entries = append(result.Entries, EntryResult{SessionID: owner.SessionID, Action: "failed", Reason: "remove directory", Bytes: bytes, Error: err.Error()})
			continue
		}
		result.Removed++
		result.ReclaimedBytes += bytes
		result.Entries = append(result.Entries, EntryResult{SessionID: owner.SessionID, Action: "removed", Bytes: bytes})
	}
	return result, nil
}

func (s *Store) lockedRoot(ctx context.Context) (string, func(), error) {
	if s.root == "" {
		return "", nil, errors.New("AO session temp root is empty")
	}
	abs, err := filepath.Abs(s.root)
	if err != nil {
		return "", nil, fmt.Errorf("resolve session temp root: %w", err)
	}
	if err := validateRootLocation(abs); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", nil, fmt.Errorf("create session temp root: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", nil, fmt.Errorf("inspect session temp root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", nil, fmt.Errorf("session temp root must be a real directory: %s", abs)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, fmt.Errorf("resolve session temp root: %w", err)
	}
	if err := validateRootLocation(root); err != nil {
		return "", nil, err
	}
	if runtime.GOOS != "windows" && (info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o700 != 0o700) {
		return "", nil, fmt.Errorf("session temp root must be private (mode 0700): %s", root)
	}
	lock, err := datadirlock.Acquire(ctx, root, 10*time.Second)
	if err != nil {
		return "", nil, fmt.Errorf("lock session temp root: %w", err)
	}
	return root, func() { _ = lock.Release() }, nil
}

func validateRootLocation(root string) error {
	root = filepath.Clean(root)
	if isFilesystemRoot(root) || samePath(root, os.TempDir()) {
		return fmt.Errorf("session temp root must be a dedicated directory, not a filesystem or global temp root: %s", root)
	}
	if home, err := os.UserHomeDir(); err == nil && samePath(root, home) {
		return fmt.Errorf("session temp root must be below the user's home directory, not the home directory itself: %s", root)
	}
	return nil
}

func isFilesystemRoot(path string) bool {
	volumeRoot := filepath.VolumeName(path) + string(os.PathSeparator)
	return samePath(path, volumeRoot)
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func readOwner(dir string) (Owner, error) {
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return Owner{}, err
	}
	if !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return Owner{}, errors.New("ownership directory is not a real directory")
	}
	if err := filepath.IsLocal(manifestName); !err {
		return Owner{}, errors.New("invalid manifest path")
	}
	manifest := filepath.Join(dir, manifestName)
	info, err := os.Lstat(manifest)
	if err != nil {
		return Owner{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Owner{}, errors.New("ownership manifest is not a regular file")
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		return Owner{}, err
	}
	var owner Owner
	if err := json.Unmarshal(data, &owner); err != nil {
		return Owner{}, fmt.Errorf("decode ownership manifest: %w", err)
	}
	if owner.Version != manifestVersion || owner.CreatedAt.IsZero() || validateSessionID(owner.SessionID) != nil {
		return Owner{}, errors.New("ownership manifest has invalid identity or version")
	}
	return owner, nil
}

func writeOwner(dir string, owner Owner) error {
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, manifestName), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func validateSessionID(id string) error {
	if strings.TrimSpace(id) == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) || strings.ContainsRune(id, 0) {
		return fmt.Errorf("invalid session id for temp storage")
	}
	return nil
}

func sessionDirName(sessionID string) string {
	digest := sha256.Sum256([]byte(sessionID))
	return "session-" + hex.EncodeToString(digest[:])
}

func containedPath(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("path is outside the owned root")
	}
	return nil
}

func treeSize(ctx context.Context, root string) (int64, error) {
	var total int64
	var count int
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > 100_000 {
			return errors.New("owned temp directory exceeds inspection limit")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink in owned temp directory")
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
