// Package agy defines AO's experimental host-trusted Antigravity reviewer.
package agy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	agentagy "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/agy"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
	"github.com/aoagents/agent-orchestrator/backend/internal/reviewgateway"
)

// HarnessID identifies the Agy reviewer adapter.
const HarnessID domain.ReviewerHarness = "agy"

const minimumVersion = "1.1.6"

// HostTrustWarning documents the authority retained by Agy's interactive TUI.
const HostTrustWarning = "experimental host-trusted reviewer: Agy sandbox mode is not OS isolation and its built-in tools retain host authority"

var requiredFlags = []string{"--agent", "--conversation", "--model", "--prompt-interactive", "--sandbox"}

// Reviewer describes Agy's host-trusted interactive lifecycle.
type Reviewer struct {
	resolveBinary func(context.Context) (string, error)
	run           func(context.Context, map[string]string, string, ...string) ([]byte, error)
	hostHome      func() (string, error)
}

// New returns the experimental host-trusted adapter.
func New() *Reviewer {
	return &Reviewer{
		resolveBinary: agentagy.ResolveAgyBinary,
		run: func(ctx context.Context, env map[string]string, binary string, args ...string) ([]byte, error) {
			cmd := aoprocess.CommandContext(ctx, binary, args...)
			cmd.Env = appendEnvironment(os.Environ(), env)
			return cmd.CombinedOutput()
		},
		hostHome: os.UserHomeDir,
	}
}

// Harness identifies the Agy reviewer.
func (*Reviewer) Harness() domain.ReviewerHarness { return HarnessID }

var _ ports.Reviewer = (*Reviewer)(nil)
var _ ports.ReviewerCanceller = (*Reviewer)(nil)
var _ ports.ReviewerRestorer = (*Reviewer)(nil)
var _ ports.ReviewerReusePolicy = (*Reviewer)(nil)
var _ ports.ReviewerStartupAuthObserver = (*Reviewer)(nil)

// ReviewProcessReusable reports false so every AGY TUI review pass launches
// fresh with the exact selected route's model and never reuses a stale pane
// running a different model.
func (*Reviewer) ReviewProcessReusable() bool { return false }

// ReviewStartupAuthHints returns bounded startup observation patterns to detect
// interactive OAuth / sign-in stalls (for example, when a bridged token is
// stale or revoked, or when the host uses unsupported keyring-only storage).
func (*Reviewer) ReviewStartupAuthHints(context.Context) (ports.StartupAuthHints, error) {
	return ports.StartupAuthHints{
		InitialDelay: 250 * time.Millisecond,
		Timeout:      3 * time.Second,
		PollInterval: 200 * time.Millisecond,
		Lines:        80,
		AuthRequiredPatterns: []string{
			"please sign in",
			"sign in to",
			"accounts.google.com",
			"authorization code",
			"oauth",
			"login required",
		},
		ReadyPatterns: []string{
			"claude-",
			"gemini-",
			"reading",
			"thinking",
		},
	}, nil
}

// ReviewCommand launches Agy's permanent interactive TUI in sandbox mode. The
// checkout and AO prompt root are explicitly added because this experimental
// mode is host-trusted rather than contained.
func (r *Reviewer) ReviewCommand(ctx context.Context, inv ports.ReviewInvocation) (ports.ReviewCommandSpec, error) {
	binary, err := r.resolveBinary(ctx)
	if err != nil {
		return ports.ReviewCommandSpec{}, err
	}
	if strings.TrimSpace(inv.TaskPromptRoot) == "" {
		return ports.ReviewCommandSpec{Argv: []string{binary}}, nil
	}
	if !filepath.IsAbs(binary) {
		return ports.ReviewCommandSpec{}, errors.New("agy reviewer: resolved binary must be absolute")
	}
	env, err := reviewgateway.PrepareHostTrustedEnvironment(inv.DataDir, inv.ReviewerID)
	if err != nil {
		return ports.ReviewCommandSpec{}, fmt.Errorf("agy reviewer: prepare Orchestrator-owned profile: %w", err)
	}
	hostTokenPath, err := r.resolveHostOAuthToken()
	if err != nil {
		return ports.ReviewCommandSpec{}, err
	}
	if err := bridgeHostOAuthToken(env.ConfigRoot, hostTokenPath); err != nil {
		return ports.ReviewCommandSpec{}, fmt.Errorf("agy reviewer: bridge host OAuth token: %w", err)
	}
	envVars := env.TUIEnvironment()
	envVars["AO_DATA_DIR"] = env.DataDir
	if err := r.verifyCompatibility(ctx, binary, envVars); err != nil {
		return ports.ReviewCommandSpec{}, err
	}
	prompt := inv.Prompt
	if strings.TrimSpace(inv.SystemPromptFile) != "" {
		prompt = "Read and follow the reviewer system instructions at `" + filepath.ToSlash(inv.SystemPromptFile) + "`, then " + prompt
	}
	return ports.ReviewCommandSpec{
		Argv:             interactiveArgv(binary, inv.WorkspacePath, inv.TaskPromptRoot, inv.Config, prompt),
		Env:              envVars,
		WorkingDirectory: inv.WorkspacePath,
	}, nil
}

// ReviewRestoreCommand restores a recorded Agy reviewer pane by relaunching
// the reviewer command with its AO-owned profile and current task context.
func (r *Reviewer) ReviewRestoreCommand(ctx context.Context, inv ports.ReviewInvocation) (ports.ReviewCommandSpec, bool, error) {
	cmd, err := r.ReviewCommand(ctx, inv)
	return cmd, true, err
}

// ReviewPreflight verifies that the Agy executable is available and that the
// host user's Antigravity OAuth token exists for bridging into the private
// reviewer profile.
func (r *Reviewer) ReviewPreflight(ctx context.Context, _ string) error {
	if _, err := r.resolveBinary(ctx); err != nil {
		return err
	}
	if _, err := r.resolveHostOAuthToken(); err != nil {
		return err
	}
	return nil
}

func (r *Reviewer) resolveHostOAuthToken() (string, error) {
	homeFn := r.hostHome
	if homeFn == nil {
		homeFn = os.UserHomeDir
	}
	home, err := homeFn()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("agy reviewer: %w: cannot resolve host home directory; run `agy` on this host to sign in", ports.ErrChatAuthRequired)
	}
	tokenPath := filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	info, err := os.Stat(tokenPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("agy reviewer: %w: host Antigravity login token not found at ~/.gemini/antigravity-cli/antigravity-oauth-token; run `agy` on this host to sign in", ports.ErrChatAuthRequired)
	}
	return tokenPath, nil
}

// bridgeHostOAuthToken copies only the host user's Antigravity OAuth token file
// into the private reviewer profile (mode 0600 inside 0700 directories) so AGY
// reuses the existing host login without sharing conversations, brain state,
// settings, or caches, and without giving the reviewer a writable symlink that
// could mutate the host user's token file.
func bridgeHostOAuthToken(configRoot, hostTokenPath string) error {
	geminiDir := filepath.Join(configRoot, ".gemini")
	cliDir := filepath.Join(geminiDir, "antigravity-cli")
	for _, dir := range []string{geminiDir, cliDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create private directory %q: %w", dir, err)
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("private path %q is not a real directory", dir)
		}
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // directories require execute permission and remain owner-only
			return fmt.Errorf("secure private directory %q: %w", dir, err)
		}
	}
	src, err := os.Open(hostTokenPath) //nolint:gosec // hostTokenPath is resolved from os.UserHomeDir and verified as a regular file
	if err != nil {
		return fmt.Errorf("open host OAuth token: %w", err)
	}
	defer func() { _ = src.Close() }()

	dst := filepath.Join(cliDir, "antigravity-oauth-token")
	if _, err := os.Lstat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("replace existing token copy: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect token copy: %w", err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // private owner-only file in AO-owned directory
	if err != nil {
		return fmt.Errorf("create private OAuth token copy: %w", err)
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy host OAuth token: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("close private OAuth token copy: %w", err)
	}
	return nil
}

func (r *Reviewer) verifyCompatibility(ctx context.Context, binary string, env map[string]string) error {
	help, err := r.run(ctx, env, binary, "--help")
	if err != nil {
		return fmt.Errorf("run agy --help: %w", err)
	}
	for _, flag := range requiredFlags {
		if !strings.Contains(string(help), flag) {
			return fmt.Errorf("installed Agy is incompatible: required flag %s is unavailable", flag)
		}
	}
	version, err := r.run(ctx, env, binary, "--version")
	if err != nil {
		return fmt.Errorf("run agy --version: %w", err)
	}
	if !versionAtLeast(strings.TrimSpace(string(version)), minimumVersion) {
		return fmt.Errorf("installed Agy %q is incompatible: version %s or newer is required", strings.TrimSpace(string(version)), minimumVersion)
	}
	return nil
}

func appendEnvironment(base []string, overrides map[string]string) []string {
	result := append([]string(nil), base...)
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

// ReviewMessage returns the short AO-owned task-file reference for a future
// long-lived TUI. The full instructions remain outside terminal scrollback.
func (*Reviewer) ReviewMessage(_ context.Context, inv ports.ReviewInvocation) (string, error) {
	return inv.Prompt, nil
}

// ReviewCancel matches Agy's fixed TUI behavior: the first Ctrl-C interrupts
// the active operation, while a second Ctrl-C begins the exit flow.
func (*Reviewer) ReviewCancel(context.Context) (ports.ReviewCancelSpec, error) {
	return ports.ReviewCancelSpec{Mode: ports.ReviewCancelInterrupt, Interrupts: 1}, nil
}

func interactiveArgv(binary, workspacePath, promptRoot string, cfg domain.AgentConfig, prompt string) []string {
	argv := []string{binary, "--sandbox"}
	if strings.TrimSpace(workspacePath) != "" {
		argv = append(argv, "--add-dir", workspacePath)
	}
	if strings.TrimSpace(promptRoot) != "" {
		argv = append(argv, "--add-dir", promptRoot)
	}
	// Forward only --model, matching the worker AGY adapter. AGY model IDs
	// already encode effort (for example claude-sonnet-5-5-low), and older
	// supported AGY versions do not guarantee a separate --effort flag.
	if model := strings.TrimSpace(cfg.Model); model != "" {
		argv = append(argv, "--model", model)
	}
	return append(argv, "--prompt-interactive", prompt)
}

func versionAtLeast(got, minimum string) bool {
	parse := func(value string) ([3]int, bool) {
		var result [3]int
		parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
		if len(parts) < 3 {
			return result, false
		}
		for i := range result {
			n, err := strconv.Atoi(parts[i])
			if err != nil {
				return result, false
			}
			result[i] = n
		}
		return result, true
	}
	a, okA := parse(got)
	b, okB := parse(minimum)
	if !okA || !okB {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}
