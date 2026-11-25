package agy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func testReviewer(help, version string) *Reviewer {
	return &Reviewer{
		resolveBinary: func(context.Context) (string, error) { return "/opt/agy/bin/agy", nil },
		run: func(_ context.Context, _ map[string]string, _ string, args ...string) ([]byte, error) {
			if slices.Equal(args, []string{"--help"}) {
				return []byte(help), nil
			}
			return []byte(version), nil
		},
		hostHome: func() (string, error) { return "", errors.New("hostHome not configured in test") },
	}
}

func testHostHomeWithToken(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	cliDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(cliDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(cliDir, "antigravity-oauth-token")
	if err := os.WriteFile(tokenPath, []byte("test-placeholder-not-a-real-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "settings.json"), []byte(`{"host":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, tokenPath
}

func TestReviewCommandPreflightShapeOnlyResolvesBinary(t *testing.T) {
	spec, err := testReviewer("", "").ReviewCommand(context.Background(), ports.ReviewInvocation{WorkspacePath: "/worker"})
	if err != nil {
		t.Fatalf("ReviewCommand: %v", err)
	}
	if !slices.Equal(spec.Argv, []string{"/opt/agy/bin/agy"}) || spec.WorkingDirectory != "" || len(spec.Env) != 0 {
		t.Fatalf("preflight spec = %+v", spec)
	}
}

func TestReviewCommandLaunchesHostTrustedInteractiveTUI(t *testing.T) {
	dataDir := t.TempDir()
	hostHome, hostTokenPath := testHostHomeWithToken(t)
	r := testReviewer(strings.Join(requiredFlags, "\n"), "1.1.9")
	r.hostHome = func() (string, error) { return hostHome, nil }
	spec, err := r.ReviewCommand(context.Background(), ports.ReviewInvocation{
		DataDir:          dataDir,
		ReviewerID:       "review-worker-1",
		TaskPromptRoot:   "/ao/prompts/worker/reviewer",
		WorkspacePath:    "/worker",
		SystemPromptFile: "/ao/prompts/worker/reviewer/system.md",
		Prompt:           "Read and follow `/ao/task.md`.",
		Config:           domain.AgentConfig{Model: "claude-sonnet-5-5-low"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPrompt := "Read and follow the reviewer system instructions at `/ao/prompts/worker/reviewer/system.md`, then Read and follow `/ao/task.md`."
	want := []string{"/opt/agy/bin/agy", "--sandbox", "--add-dir", "/worker", "--add-dir", "/ao/prompts/worker/reviewer", "--model", "claude-sonnet-5-5-low", "--prompt-interactive", wantPrompt}
	if !slices.Equal(spec.Argv, want) || spec.WorkingDirectory != "/worker" {
		t.Fatalf("spec = %+v, want argv %#v", spec, want)
	}
	configRoot := filepath.Join(dataDir, "reviewer-runtime", "review-worker-1", "config")
	if spec.Env["HOME"] != configRoot || spec.Env["AO_DATA_DIR"] != dataDir {
		t.Fatalf("AO-owned environment = %#v", spec.Env)
	}
	// Verify private profile isolation: only a private 0600 copy of the OAuth
	// token is created (never a writable symlink to the host token), .gemini
	// and antigravity-cli are real private dirs, and host settings are absent.
	for _, dir := range []string{filepath.Join(configRoot, ".gemini"), filepath.Join(configRoot, ".gemini", "antigravity-cli")} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("expected real private directory at %q, got info=%v err=%v", dir, info, err)
		}
	}
	dstToken := filepath.Join(configRoot, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	dstInfo, err := os.Lstat(dstToken)
	if err != nil || !dstInfo.Mode().IsRegular() || dstInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected regular private copy at %q (not symlink), got info=%v err=%v", dstToken, dstInfo, err)
	}
	if dstInfo.Mode().Perm() != 0o600 {
		t.Fatalf("private token copy perms = %o, want 0600", dstInfo.Mode().Perm())
	}
	gotBytes, err := os.ReadFile(dstToken)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := os.ReadFile(hostTokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBytes) != string(wantBytes) {
		t.Fatalf("private token copy mismatch")
	}
	if _, err := os.Lstat(filepath.Join(configRoot, ".gemini", "antigravity-cli", "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host settings.json must not be bridged into private profile, err=%v", err)
	}
}

func TestReviewCommandAndRestorePropagateModelWithoutInventingEffort(t *testing.T) {
	dataDir := t.TempDir()
	hostHome, _ := testHostHomeWithToken(t)
	// Supported CLI help includes --model and omits --effort.
	r := testReviewer(strings.Join(requiredFlags, "\n"), "1.1.9")
	r.hostHome = func() (string, error) { return hostHome, nil }
	inv := ports.ReviewInvocation{
		DataDir:        dataDir,
		ReviewerID:     "review-worker-2",
		TaskPromptRoot: "/ao/prompts/worker/reviewer",
		WorkspacePath:  "/worker",
		Prompt:         "Read and follow `/ao/task.md`.",
		Config:         domain.AgentConfig{Model: "claude-sonnet-5-5-low", Effort: "low"},
	}
	cmd, err := r.ReviewCommand(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	restoreCmd, ok, err := r.ReviewRestoreCommand(context.Background(), inv)
	if err != nil || !ok {
		t.Fatalf("ReviewRestoreCommand = ok:%v err:%v", ok, err)
	}
	want := []string{"/opt/agy/bin/agy", "--sandbox", "--add-dir", "/worker", "--add-dir", "/ao/prompts/worker/reviewer", "--model", "claude-sonnet-5-5-low", "--prompt-interactive", "Read and follow `/ao/task.md`."}
	if !slices.Equal(cmd.Argv, want) {
		t.Fatalf("ReviewCommand argv = %#v, want %#v", cmd.Argv, want)
	}
	if !slices.Equal(restoreCmd.Argv, want) {
		t.Fatalf("ReviewRestoreCommand argv = %#v, want %#v", restoreCmd.Argv, want)
	}
	if r.ReviewProcessReusable() {
		t.Fatal("ReviewProcessReusable() = true, want false so stale panes cannot run a different model")
	}
}

func TestReviewPreflightAndCommandFailClosedWithoutHostAuth(t *testing.T) {
	emptyHome := t.TempDir()
	r := testReviewer(strings.Join(requiredFlags, "\n"), "1.5.16")
	r.hostHome = func() (string, error) { return emptyHome, nil }

	err := r.ReviewPreflight(context.Background(), "/worker")
	if !errors.Is(err, ports.ErrChatAuthRequired) {
		t.Fatalf("ReviewPreflight err = %v, want ErrChatAuthRequired", err)
	}
	if domain.IsQuotaFailedReviewRun(domain.ReviewRun{Status: domain.ReviewRunFailed, Body: err.Error()}) {
		t.Fatal("auth-required failure must not be classified as quota exhaustion")
	}
	_, err = r.ReviewCommand(context.Background(), ports.ReviewInvocation{
		DataDir:        t.TempDir(),
		ReviewerID:     "review-worker-3",
		TaskPromptRoot: "/ao/prompts/worker/reviewer",
		WorkspacePath:  "/worker",
		Prompt:         "Read and follow `/ao/task.md`.",
	})
	if !errors.Is(err, ports.ErrChatAuthRequired) {
		t.Fatalf("ReviewCommand err = %v, want ErrChatAuthRequired", err)
	}

	// Recovery: once host auth token appears, preflight succeeds.
	cliDir := filepath.Join(emptyHome, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(cliDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "antigravity-oauth-token"), []byte("recovered-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.ReviewPreflight(context.Background(), "/worker"); err != nil {
		t.Fatalf("ReviewPreflight after auth recovery = %v, want nil", err)
	}
}

func TestInteractiveArgvIsPermanentTUIOnly(t *testing.T) {
	argv := interactiveArgv("agy", "/worker", "/ao/prompts", domain.AgentConfig{Model: "claude-sonnet-5-5-low", Effort: "unsupported-effort"}, "Read and follow `/ao/task.md`.")
	want := []string{"agy", "--sandbox", "--add-dir", "/worker", "--add-dir", "/ao/prompts", "--model", "claude-sonnet-5-5-low", "--prompt-interactive", "Read and follow `/ao/task.md`."}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv = %#v, want %#v", argv, want)
	}
	for _, forbidden := range []string{"--print", "-p", "--prompt", "--output-format", "json", "stream-json", "rpc", "--effort"} {
		if slices.Contains(argv, forbidden) {
			t.Fatalf("interactive argv contains forbidden %q: %#v", forbidden, argv)
		}
	}
}

func TestCompatibilityProbeRequiresCurrentInteractiveSurface(t *testing.T) {
	help := strings.Join(requiredFlags, "\n")
	r := testReviewer(help, "1.1.9")
	if err := r.verifyCompatibility(context.Background(), "/opt/agy/bin/agy", map[string]string{"HOME": "/ao/profile"}); err != nil {
		t.Fatalf("verifyCompatibility: %v", err)
	}

	missing := testReviewer(strings.Replace(help, "--prompt-interactive", "", 1), "1.1.9")
	if err := missing.verifyCompatibility(context.Background(), "/opt/agy/bin/agy", nil); err == nil || !strings.Contains(err.Error(), "--prompt-interactive") {
		t.Fatalf("missing flag err = %v", err)
	}
	old := testReviewer(help, "1.1.5")
	if err := old.verifyCompatibility(context.Background(), "/opt/agy/bin/agy", nil); err == nil || !strings.Contains(err.Error(), minimumVersion) {
		t.Fatalf("old version err = %v", err)
	}
}

func TestProductionPreflightResolvesHostTrustedCLI(t *testing.T) {
	hostHome, _ := testHostHomeWithToken(t)
	r := testReviewer("", "")
	r.hostHome = func() (string, error) { return hostHome, nil }
	if err := r.ReviewPreflight(context.Background(), "/worker"); err != nil {
		t.Fatalf("ReviewPreflight err = %v", err)
	}
}

func TestReviewMessageAndSingleInterruptCancel(t *testing.T) {
	r := testReviewer("", "")
	message, err := r.ReviewMessage(context.Background(), ports.ReviewInvocation{Prompt: "next task"})
	if err != nil || message != "next task" {
		t.Fatalf("ReviewMessage = %q, %v", message, err)
	}
	cancel, err := r.ReviewCancel(context.Background())
	if err != nil || cancel.Mode != ports.ReviewCancelInterrupt || cancel.Interrupts != 1 {
		t.Fatalf("ReviewCancel = %+v, %v", cancel, err)
	}
}

func TestHostTrustWarningIsExplicit(t *testing.T) {
	for _, phrase := range []string{"host-trusted", "not OS isolation", "built-in tools"} {
		if !strings.Contains(HostTrustWarning, phrase) {
			t.Fatalf("warning %q missing %q", HostTrustWarning, phrase)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	for _, tc := range []struct {
		got  string
		want bool
	}{{"1.1.6", true}, {"v1.1.9", true}, {"1.2.0", true}, {"2.0.0", true}, {"1.1.5", false}, {"invalid", false}} {
		if got := versionAtLeast(tc.got, minimumVersion); got != tc.want {
			t.Errorf("versionAtLeast(%q) = %v, want %v", tc.got, got, tc.want)
		}
	}
}

func TestReviewerImplementsChatProfileForAgyACP(t *testing.T) {
	r := New()
	if got := r.ReviewChatHarness(); got != domain.HarnessAgy {
		t.Fatalf("ReviewChatHarness() = %q, want %q", got, domain.HarnessAgy)
	}
}
