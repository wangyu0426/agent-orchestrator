// Package reviewgateway prepares AO-owned runtime directories and immutable
// task manifests used by interactive reviewer TUIs.
package reviewgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const (
	manifestVersion = 1
	resultVersion   = 1
	maxResultBytes  = 1 << 20

	ManifestPointerFilename = "pi-active-manifest"
	ResultPendingDirName    = "pending"
	ResultReceiptsDirName   = "receipts"
	EnvManifestPointer      = "AO_REVIEW_GATEWAY_MANIFEST_POINTER"
	EnvResultPendingDir     = "AO_REVIEW_RESULT_PENDING_DIRECTORY"
)

var (
	safeID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	commitSHA  = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	githubID   = regexp.MustCompile(`^[0-9]+$`)
	resultHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Task is one immutable review capability.
type Task struct {
	RunID          string `json:"runId"`
	PRURL          string `json:"prUrl"`
	TargetSHA      string `json:"targetSha"`
	TaskPromptFile string `json:"taskPromptFile"`
}

// Manifest authorizes a reviewer process for one worker and an exact set of
// review runs. WorkspacePath is host-side gateway state and is never intended
// to be exposed as the TUI's working directory.
type Manifest struct {
	Version         int              `json:"version"`
	ReviewerID      string           `json:"reviewerId"`
	ReviewSessionID string           `json:"reviewSessionId"`
	WorkerSessionID domain.SessionID `json:"workerSessionId"`
	WorkspacePath   string           `json:"workspacePath"`
	TaskPromptRoot  string           `json:"taskPromptRoot"`
	Tasks           []Task           `json:"tasks"`
}

// Environment is the neutral AO-owned filesystem presented to a reviewer.
// The source checkout is intentionally absent.
type Environment struct {
	DataDir           string
	Root              string
	WorkingDirectory  string
	ConfigRoot        string
	StateRoot         string
	CacheRoot         string
	TempRoot          string
	ManifestPath      string
	ResultPendingDir  string
	ResultReceiptsDir string
}

// Result is the structured verdict a reviewer leaves for AO. Identity fields
// are copied from the immutable manifest; only Verdict, Body, and
// GithubReviewID are reviewer-supplied values.
type Result struct {
	Version         int    `json:"version"`
	ReviewerID      string `json:"reviewerId"`
	ReviewSessionID string `json:"reviewSessionId"`
	WorkerSessionID string `json:"workerSessionId"`
	RunID           string `json:"runId"`
	PRURL           string `json:"prUrl"`
	TargetSHA       string `json:"targetSha"`
	Verdict         string `json:"verdict"`
	GithubReviewID  string `json:"githubReviewId"`
	Body            string `json:"body"`
}

// Submission contains only reviewer-selected fields. QueueResult derives all
// session, run, PR, and HEAD identity from the active manifest.
type Submission struct {
	RunID          string
	Verdict        string
	Body           string
	GithubReviewID string
}

// ResultReceipt is written by the trusted host after the review service has
// committed or rejected a result.
type ResultReceipt struct {
	Version    int    `json:"version"`
	RunID      string `json:"runId"`
	ResultHash string `json:"resultHash"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
}

// PendingResult is one atomically published reviewer result. InvalidReason is
// populated only for a stable, malformed inbox file; incomplete writes are
// retried before they are rejected.
type PendingResult struct {
	ReviewerID    string
	RunID         string
	Result        Result
	ResultHash    string
	InvalidReason string
}

// ResultInbox reads and acknowledges the narrow filesystem mailbox shared by
// an isolated reviewer and the trusted AO host.
type ResultInbox struct {
	dataDir string
}

// TUIEnvironment returns provider-neutral directory overrides for an adapter's
// RuntimeConfig.Env. Provider-specific adapters may add only documented flags;
// the eventual OS sandbox remains responsible for excluding inherited host state.
func (e Environment) TUIEnvironment() map[string]string {
	return map[string]string{
		"HOME": e.ConfigRoot, "XDG_CONFIG_HOME": e.ConfigRoot,
		"XDG_STATE_HOME": e.StateRoot, "XDG_CACHE_HOME": e.CacheRoot,
		"TMPDIR": e.TempRoot, "TEMP": e.TempRoot, "TMP": e.TempRoot,
	}
}

// PrepareHostTrustedEnvironment creates AO-owned discovery and state roots for
// an experimental host-trusted reviewer. It limits where the CLI stores its own
// files but intentionally does not claim process, filesystem, or network
// containment.
func PrepareHostTrustedEnvironment(dataDir, reviewerID string) (Environment, error) {
	if strings.TrimSpace(dataDir) == "" || !filepath.IsAbs(dataDir) {
		return Environment{}, errors.New("review gateway: absolute Orchestrator data directory is required")
	}
	if !safeID.MatchString(reviewerID) {
		return Environment{}, errors.New("review gateway: invalid reviewer id")
	}
	root := filepath.Join(dataDir, "reviewer-runtime", reviewerID)
	env := Environment{
		DataDir: dataDir, Root: root,
		WorkingDirectory: filepath.Join(root, "workspace"),
		ConfigRoot:       filepath.Join(root, "config"), StateRoot: filepath.Join(root, "state"),
		CacheRoot: filepath.Join(root, "cache"), TempRoot: filepath.Join(root, "tmp"),
	}
	for _, dir := range []string{env.Root, env.WorkingDirectory, env.ConfigRoot, env.StateRoot, env.CacheRoot, env.TempRoot} {
		if err := ensurePrivateDir(dir); err != nil {
			return Environment{}, err
		}
	}
	return env, nil
}

// PrepareEnvironment creates a private reviewer root under AO_DATA_DIR and an
// immutable, content-addressed authorization manifest. It never creates files
// in the project checkout.
func PrepareEnvironment(dataDir string, manifest Manifest) (Environment, error) {
	if strings.TrimSpace(dataDir) == "" {
		return Environment{}, errors.New("review gateway: Orchestrator data directory is required")
	}
	if !filepath.IsAbs(dataDir) {
		return Environment{}, errors.New("review gateway: Orchestrator data directory must be absolute")
	}
	if err := validateManifest(&manifest); err != nil {
		return Environment{}, err
	}
	root := filepath.Join(dataDir, "reviewer-runtime", manifest.ReviewerID)
	env := Environment{
		DataDir:           dataDir,
		Root:              root,
		WorkingDirectory:  filepath.Join(root, "workspace"),
		ConfigRoot:        filepath.Join(root, "config"),
		StateRoot:         filepath.Join(root, "state"),
		CacheRoot:         filepath.Join(root, "cache"),
		TempRoot:          filepath.Join(root, "tmp"),
		ResultPendingDir:  filepath.Join(root, "results", ResultPendingDirName),
		ResultReceiptsDir: filepath.Join(root, "results", ResultReceiptsDirName),
	}
	for _, dir := range []string{filepath.Join(dataDir, "reviewer-runtime"), env.Root, env.WorkingDirectory, env.ConfigRoot, env.StateRoot, env.CacheRoot, env.TempRoot, filepath.Join(root, "manifests"), filepath.Join(root, "disabled-git-hooks"), filepath.Join(root, "results"), env.ResultPendingDir, env.ResultReceiptsDir} {
		if err := ensurePrivateDir(dir); err != nil {
			return Environment{}, err
		}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return Environment{}, fmt.Errorf("review gateway: encode manifest: %w", err)
	}
	sum := sha256.Sum256(raw)
	env.ManifestPath = filepath.Join(root, "manifests", hex.EncodeToString(sum[:])+".json")
	if prior, readErr := os.ReadFile(env.ManifestPath); readErr == nil {
		if !bytes.Equal(prior, raw) {
			return Environment{}, errors.New("review gateway: content-addressed manifest collision")
		}
		return env, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return Environment{}, fmt.Errorf("review gateway: inspect manifest: %w", readErr)
	}
	f, err := os.OpenFile(env.ManifestPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Environment{}, fmt.Errorf("review gateway: create manifest: %w", err)
	}
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return Environment{}, fmt.Errorf("review gateway: write manifest: %w", err)
	}
	if closeErr != nil {
		return Environment{}, fmt.Errorf("review gateway: close manifest: %w", closeErr)
	}
	return env, nil
}

// PrepareActiveEnvironment publishes the immutable task manifest through the
// stable pointer used by a reviewer process. Replacing the pointer is atomic so
// a long-lived reviewer sees either the previous complete manifest or the new
// complete one while AO queues another task.
func PrepareActiveEnvironment(dataDir string, manifest Manifest) (Environment, string, error) {
	env, err := PrepareEnvironment(dataDir, manifest)
	if err != nil {
		return Environment{}, "", err
	}
	pointerPath := filepath.Join(env.Root, ManifestPointerFilename)
	if err := writeAtomicFile(pointerPath, []byte(env.ManifestPath+"\n"), 0o600); err != nil {
		return Environment{}, "", fmt.Errorf("review gateway: publish active manifest: %w", err)
	}
	return env, pointerPath, nil
}

// PrepareResultEnvironment creates the bounded writable mailbox used by a
// reviewer result transport, including when AO restores an idle reviewer.
func PrepareResultEnvironment(dataDir, reviewerID string) (Environment, error) {
	if strings.TrimSpace(dataDir) == "" || !filepath.IsAbs(dataDir) {
		return Environment{}, errors.New("review gateway: absolute Orchestrator data directory is required")
	}
	if !safeID.MatchString(reviewerID) {
		return Environment{}, errors.New("review gateway: invalid reviewer id")
	}
	root := filepath.Join(dataDir, "reviewer-runtime", reviewerID)
	env := Environment{
		DataDir: dataDir, Root: root,
		ResultPendingDir:  filepath.Join(root, "results", ResultPendingDirName),
		ResultReceiptsDir: filepath.Join(root, "results", ResultReceiptsDirName),
	}
	for _, dir := range []string{filepath.Join(dataDir, "reviewer-runtime"), root, filepath.Join(root, "results"), env.ResultPendingDir, env.ResultReceiptsDir} {
		if err := ensurePrivateDir(dir); err != nil {
			return Environment{}, err
		}
	}
	return env, nil
}

// NewResultInbox constructs the host-side view of AO-owned reviewer mailboxes.
func NewResultInbox(dataDir string) (*ResultInbox, error) {
	if strings.TrimSpace(dataDir) == "" || !filepath.IsAbs(dataDir) {
		return nil, errors.New("review gateway: absolute Orchestrator data directory is required")
	}
	return &ResultInbox{dataDir: filepath.Clean(dataDir)}, nil
}

// LoadManifest reads and verifies the active immutable manifest pointer that
// AO gives to a reviewer. It rejects symlinks, paths outside the reviewer's
// manifest directory, and content whose filename does not match its hash.
func LoadManifest(pointerPath string) (Manifest, error) {
	if !filepath.IsAbs(pointerPath) || filepath.Base(pointerPath) != ManifestPointerFilename {
		return Manifest{}, errors.New("review gateway: invalid active manifest pointer path")
	}
	pointerInfo, err := os.Lstat(pointerPath)
	if err != nil || !pointerInfo.Mode().IsRegular() || pointerInfo.Mode()&os.ModeSymlink != 0 {
		return Manifest{}, errors.New("review gateway: active manifest pointer is not a regular file")
	}
	pointer, err := os.ReadFile(pointerPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("review gateway: read active manifest pointer: %w", err)
	}
	manifestPath := strings.TrimSpace(string(pointer))
	if !filepath.IsAbs(manifestPath) {
		return Manifest{}, errors.New("review gateway: active manifest path must be absolute")
	}
	reviewerRoot := filepath.Dir(pointerPath)
	manifestRoot := filepath.Join(reviewerRoot, "manifests")
	manifestPath = filepath.Clean(manifestPath)
	if !pathWithin(manifestRoot, manifestPath) || filepath.Dir(manifestPath) != manifestRoot {
		return Manifest{}, errors.New("review gateway: active manifest is outside its immutable manifest directory")
	}
	info, err := os.Lstat(manifestPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Manifest{}, errors.New("review gateway: active manifest is not a regular file")
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("review gateway: read active manifest: %w", err)
	}
	sum := sha256.Sum256(raw)
	if filepath.Base(manifestPath) != hex.EncodeToString(sum[:])+".json" {
		return Manifest{}, errors.New("review gateway: active manifest content hash does not match its filename")
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("review gateway: decode active manifest: %w", err)
	}
	if err := validateManifest(&manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.ReviewerID != filepath.Base(reviewerRoot) {
		return Manifest{}, errors.New("review gateway: active manifest reviewer does not match its directory")
	}
	return manifest, nil
}

// QueueResult atomically writes one reviewer result into the host inbox. It
// uses the immutable manifest for every identity field so the reviewer cannot
// redirect a verdict to another worker, PR, run, or commit.
func (i *ResultInbox) QueueResult(manifest Manifest, submission Submission) (PendingResult, error) {
	if i == nil || i.dataDir == "" {
		return PendingResult{}, errors.New("review gateway: result inbox is not configured")
	}
	if err := validateManifest(&manifest); err != nil {
		return PendingResult{}, err
	}
	var task *Task
	for index := range manifest.Tasks {
		if manifest.Tasks[index].RunID == submission.RunID {
			task = &manifest.Tasks[index]
			break
		}
	}
	if task == nil {
		return PendingResult{}, errors.New("review gateway: run is not authorized by the active manifest")
	}
	result := Result{
		Version: resultVersion, ReviewerID: manifest.ReviewerID, ReviewSessionID: manifest.ReviewSessionID,
		WorkerSessionID: string(manifest.WorkerSessionID), RunID: task.RunID,
		PRURL: task.PRURL, TargetSHA: task.TargetSHA, Verdict: submission.Verdict,
		GithubReviewID: submission.GithubReviewID, Body: submission.Body,
	}
	if err := validateResult(result); err != nil {
		return PendingResult{}, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return PendingResult{}, fmt.Errorf("review gateway: encode result: %w", err)
	}
	if len(raw) > maxResultBytes {
		return PendingResult{}, errors.New("review gateway: result is too large")
	}
	hash := sha256.Sum256(raw)
	item := PendingResult{ReviewerID: manifest.ReviewerID, RunID: result.RunID, Result: result, ResultHash: hex.EncodeToString(hash[:])}
	pendingDir := i.pendingDir(manifest.ReviewerID)
	if err := ensurePrivateDir(pendingDir); err != nil {
		return PendingResult{}, err
	}
	path := filepath.Join(pendingDir, result.RunID+".json")
	if prior, readErr := os.ReadFile(path); readErr == nil {
		if !bytes.Equal(prior, raw) {
			return PendingResult{}, errors.New("review gateway: a different result is already pending for this run")
		}
		return item, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return PendingResult{}, fmt.Errorf("review gateway: inspect pending result: %w", readErr)
	}
	if err := writeNewResultFile(path, raw); err != nil {
		if errors.Is(err, os.ErrExist) {
			prior, readErr := os.ReadFile(path)
			if readErr == nil && bytes.Equal(prior, raw) {
				return item, nil
			}
			return PendingResult{}, errors.New("review gateway: a different result is already pending for this run")
		}
		return PendingResult{}, fmt.Errorf("review gateway: queue result: %w", err)
	}
	return item, nil
}

// WaitForReceipt waits for the trusted host to acknowledge this exact payload.
// A timeout leaves the result in the inbox so a later host scan can deliver it.
func (i *ResultInbox) WaitForReceipt(ctx context.Context, item PendingResult, interval time.Duration) (ResultReceipt, error) {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	path, err := i.receiptPath(item.ReviewerID, item.RunID, item.ResultHash)
	if err != nil {
		return ResultReceipt{}, err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if receipt, ok, readErr := readReceipt(path); readErr != nil {
			return ResultReceipt{}, readErr
		} else if ok {
			if receipt.Version != resultVersion || receipt.RunID != item.RunID || receipt.ResultHash != item.ResultHash {
				return ResultReceipt{}, errors.New("review gateway: receipt does not match the submitted result")
			}
			return receipt, nil
		}
		select {
		case <-ctx.Done():
			return ResultReceipt{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Pending returns ready result files from every reviewer mailbox. Partial or
// malformed writes are left alone briefly; a stable malformed file is exposed
// for rejection and cleanup by the service.
func (i *ResultInbox) Pending(ctx context.Context) ([]PendingResult, error) {
	if i == nil || i.dataDir == "" {
		return nil, errors.New("review gateway: result inbox is not configured")
	}
	root := filepath.Join(i.dataDir, "reviewer-runtime")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("review gateway: list reviewer result roots: %w", err)
	}
	var out []PendingResult
	for _, reviewerEntry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reviewerID := reviewerEntry.Name()
		if !safeID.MatchString(reviewerID) || !reviewerEntry.IsDir() {
			continue
		}
		reviewerRoot := filepath.Join(root, reviewerID)
		if info, statErr := os.Lstat(reviewerRoot); statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		pendingDir := i.pendingDir(reviewerID)
		files, readErr := os.ReadDir(pendingDir)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("review gateway: list pending results for %s: %w", reviewerID, readErr)
		}
		for _, entry := range files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			runID := strings.TrimSuffix(entry.Name(), ".json")
			if !safeID.MatchString(runID) {
				continue
			}
			path := filepath.Join(pendingDir, entry.Name())
			info, statErr := os.Lstat(path)
			if statErr != nil {
				continue
			}
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				// A sandbox process can only create a result entry in its own
				// mailbox. Remove unsupported entries rather than letting one
				// block delivery of otherwise valid results indefinitely.
				_ = os.Remove(path)
				continue
			}
			if info.Size() > maxResultBytes {
				_ = os.Remove(path)
				continue
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				continue
			}
			hash := sha256.Sum256(raw)
			item := PendingResult{ReviewerID: reviewerID, RunID: runID, ResultHash: hex.EncodeToString(hash[:])}
			if len(raw) > maxResultBytes {
				item.InvalidReason = "result is too large"
			} else if err := decodeResult(raw, &item.Result); err != nil {
				if time.Since(info.ModTime()) < 2*time.Second {
					continue
				}
				item.InvalidReason = err.Error()
			} else if item.Result.RunID != runID || item.Result.ReviewerID != reviewerID {
				item.InvalidReason = "result identity does not match its inbox path"
			} else if err := validateResult(item.Result); err != nil {
				item.InvalidReason = err.Error()
			}
			out = append(out, item)
		}
	}
	return out, nil
}

// HasPendingResults reports whether any named run has a published inbox file.
// The review engine uses this before treating a dead reviewer as a failed run.
func (i *ResultInbox) HasPendingResults(ctx context.Context, reviewerID string, runs []domain.ReviewRun) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !safeID.MatchString(reviewerID) {
		return false, errors.New("review gateway: invalid reviewer id")
	}
	for _, run := range runs {
		if !safeID.MatchString(run.ID) || run.Status != domain.ReviewRunRunning {
			continue
		}
		path := filepath.Join(i.pendingDir(reviewerID), run.ID+".json")
		info, err := os.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxResultBytes {
				continue
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return false, readErr
			}
			var result Result
			if decodeResult(raw, &result) != nil || validateResult(result) != nil {
				if time.Since(info.ModTime()) < 2*time.Second {
					return true, nil
				}
				continue
			}
			if result.ReviewerID == reviewerID && result.ReviewSessionID == run.ReviewID && result.WorkerSessionID == string(run.SessionID) && result.RunID == run.ID && result.PRURL == run.PRURL && result.TargetSHA == run.TargetSHA {
				return true, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// Acknowledge writes the receipt before removing the pending file. Replaying a
// result after a daemon restart is safe because service submission is
// idempotent and this method is idempotent too.
func (i *ResultInbox) Acknowledge(item PendingResult, status, message string) error {
	if status != "delivered" && status != "rejected" {
		return errors.New("review gateway: invalid receipt status")
	}
	path, err := i.receiptPath(item.ReviewerID, item.RunID, item.ResultHash)
	if err != nil {
		return err
	}
	if len(message) > 1000 {
		message = message[:1000]
	}
	receipt := ResultReceipt{Version: resultVersion, RunID: item.RunID, ResultHash: item.ResultHash, Status: status, Message: message}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("review gateway: encode result receipt: %w", err)
	}
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	if prior, readErr := os.ReadFile(path); readErr == nil {
		var existing ResultReceipt
		if json.Unmarshal(prior, &existing) != nil || existing != receipt {
			return errors.New("review gateway: conflicting result receipt already exists")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("review gateway: inspect result receipt: %w", readErr)
	} else if err := writeAtomicFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("review gateway: write result receipt: %w", err)
	}
	pendingPath := filepath.Join(i.pendingDir(item.ReviewerID), item.RunID+".json")
	if pending, readErr := os.ReadFile(pendingPath); readErr == nil {
		sum := sha256.Sum256(pending)
		if hex.EncodeToString(sum[:]) == item.ResultHash {
			if err := os.Remove(pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("review gateway: remove acknowledged result: %w", err)
			}
		}
	}
	return nil
}

func (i *ResultInbox) pendingDir(reviewerID string) string {
	return filepath.Join(i.dataDir, "reviewer-runtime", reviewerID, "results", ResultPendingDirName)
}

func (i *ResultInbox) receiptPath(reviewerID, runID, hash string) (string, error) {
	if !safeID.MatchString(reviewerID) || !safeID.MatchString(runID) || !resultHash.MatchString(hash) {
		return "", errors.New("review gateway: invalid result receipt identity")
	}
	return filepath.Join(i.dataDir, "reviewer-runtime", reviewerID, "results", ResultReceiptsDirName, runID, hash+".json"), nil
}

func readReceipt(path string) (ResultReceipt, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ResultReceipt{}, false, nil
	}
	if err != nil {
		return ResultReceipt{}, false, fmt.Errorf("review gateway: read result receipt: %w", err)
	}
	var receipt ResultReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return ResultReceipt{}, false, fmt.Errorf("review gateway: decode result receipt: %w", err)
	}
	return receipt, true, nil
}

func validateResult(result Result) error {
	if result.Version != resultVersion || !safeID.MatchString(result.ReviewerID) || !safeID.MatchString(result.ReviewSessionID) || strings.TrimSpace(result.WorkerSessionID) == "" || !safeID.MatchString(result.RunID) || !commitSHA.MatchString(result.TargetSHA) {
		return errors.New("review gateway: invalid result identity")
	}
	if _, _, err := parsePRURL(result.PRURL); err != nil {
		return err
	}
	if result.Verdict != "approved" && result.Verdict != "changes_requested" {
		return errors.New("review gateway: invalid review verdict")
	}
	if result.Verdict == "changes_requested" && result.Body == "" {
		return errors.New("review gateway: changes_requested result requires a body")
	}
	if result.GithubReviewID != "" && !githubID.MatchString(result.GithubReviewID) {
		return errors.New("review gateway: invalid GitHub review id")
	}
	return nil
}

func decodeResult(raw []byte, out *Result) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("review gateway: trailing data after result")
	}
	return nil
}

func writeNewResultFile(path string, raw []byte) error {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".result-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	// Hard-linking publishes the complete file without replacing an existing
	// result. Fall back to O_EXCL on filesystems that do not support hard links;
	// host readers ignore a partially written JSON file until it is stable.
	if err := os.Link(tempPath, path); err == nil {
		return nil
	} else if errors.Is(err, os.ErrExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return closeErr
	}
	return nil
}

func writeAtomicFile(path string, raw []byte, mode os.FileMode) error {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".receipt-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func validateManifest(manifest *Manifest) error {
	if manifest.Version != 0 && manifest.Version != manifestVersion {
		return errors.New("review gateway: unsupported manifest version")
	}
	manifest.Version = manifestVersion
	if !safeID.MatchString(manifest.ReviewerID) || !safeID.MatchString(manifest.ReviewSessionID) || strings.TrimSpace(string(manifest.WorkerSessionID)) == "" {
		return errors.New("review gateway: invalid reviewer or worker id")
	}
	if !filepath.IsAbs(manifest.WorkspacePath) || !filepath.IsAbs(manifest.TaskPromptRoot) || len(manifest.Tasks) == 0 {
		return errors.New("review gateway: absolute workspace, prompt root, and tasks are required")
	}
	seen := make(map[string]bool, len(manifest.Tasks))
	for _, task := range manifest.Tasks {
		if !safeID.MatchString(task.RunID) || seen[task.RunID] || !commitSHA.MatchString(task.TargetSHA) {
			return errors.New("review gateway: invalid or duplicate task identity")
		}
		if _, _, err := parsePRURL(task.PRURL); err != nil {
			return err
		}
		prompt, err := filepath.Abs(task.TaskPromptFile)
		if err != nil || !pathWithin(manifest.TaskPromptRoot, prompt) {
			return errors.New("review gateway: task prompt is outside the Orchestrator prompt root")
		}
		seen[task.RunID] = true
	}
	return nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func parsePRURL(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("review gateway: PR URL must be an https github.com pull URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" || !validGitHubSegment(parts[0]) || !validGitHubSegment(parts[1]) {
		return "", "", errors.New("review gateway: invalid GitHub pull URL")
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n < 1 {
		return "", "", errors.New("review gateway: invalid pull request number")
	}
	return parts[0] + "/" + parts[1], strconv.Itoa(n), nil
}

func validGitHubSegment(segment string) bool {
	if segment == "." || segment == ".." || segment == "" {
		return false
	}
	for _, r := range segment {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("review gateway: create private directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("review gateway: private path is not a real directory")
	}
	if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // directories require execute permission and remain owner-only
		return fmt.Errorf("review gateway: secure private directory: %w", err)
	}
	return nil
}
