package githubbroker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWorkerEnv_ScrubsPersonalTokensAndRewritesSSH(t *testing.T) {
	env := WorkerEnv(WorkerEnvOptions{
		BrokerToken: "sess-cap-token",
		GitHubHost:  "github.com",
		AOBinary:    "/usr/local/bin/ao",
		GHConfigDir: "/tmp/gh-iso",
		GHShimDir:   "/tmp/github-broker-bin/sess-1",
		CurrentPATH: "/usr/bin:/bin",
	})

	for _, k := range InheritedCredentialEnvKeys {
		if k == "GIT_SSH_COMMAND" {
			if !strings.Contains(env[k], "github-broker ssh-guard") {
				t.Fatalf("expected GIT_SSH_COMMAND to invoke ssh-guard, got %q", env[k])
			}
			continue
		}
		if v, ok := env[k]; !ok || v != "" {
			t.Fatalf("expected inherited credential %s to be scrubbed to empty, got %q (present=%v)", k, v, ok)
		}
	}
	if env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("expected GIT_TERMINAL_PROMPT=0, got %q", env["GIT_TERMINAL_PROMPT"])
	}
	if env["GIT_CONFIG_NOSYSTEM"] != "" || env["GIT_CONFIG_GLOBAL"] != "" {
		t.Fatalf("expected non-GitHub global/system git config to remain preserved, got NOSYSTEM=%q GLOBAL=%q", env["GIT_CONFIG_NOSYSTEM"], env["GIT_CONFIG_GLOBAL"])
	}
	if env[EnvBrokerToken] != "sess-cap-token" {
		t.Fatalf("expected %s to be set, got %q", EnvBrokerToken, env[EnvBrokerToken])
	}
	if env["GH_CONFIG_DIR"] != "/tmp/gh-iso" {
		t.Fatalf("expected isolated GH_CONFIG_DIR, got %q", env["GH_CONFIG_DIR"])
	}
	if !strings.HasPrefix(env["PATH"], "/tmp/github-broker-bin/sess-1") {
		t.Fatalf("expected GHShimDir prepended to PATH, got %q", env["PATH"])
	}
	if env["GIT_CONFIG_KEY_0"] != "credential.https://github.com.helper" || env["GIT_CONFIG_VALUE_0"] != "" {
		t.Fatalf("expected first git config to clear host credential.helper, got %q=%q", env["GIT_CONFIG_KEY_0"], env["GIT_CONFIG_VALUE_0"])
	}
	if !strings.Contains(env["GIT_CONFIG_VALUE_1"], "github-broker git-credential") {
		t.Fatalf("expected broker credential helper, got %q", env["GIT_CONFIG_VALUE_1"])
	}
	if env["GIT_CONFIG_KEY_2"] != "credential.https://github.com.useHttpPath" || env["GIT_CONFIG_VALUE_2"] != "true" {
		t.Fatalf("expected host credential.useHttpPath=true, got %q=%q", env["GIT_CONFIG_KEY_2"], env["GIT_CONFIG_VALUE_2"])
	}
	if env["GIT_CONFIG_KEY_3"] != "http.https://github.com/.extraHeader" || env["GIT_CONFIG_VALUE_3"] != "" {
		t.Fatalf("expected host http.extraHeader reset, got %q=%q", env["GIT_CONFIG_KEY_3"], env["GIT_CONFIG_VALUE_3"])
	}
	if env["GIT_CONFIG_KEY_5"] != "url.https://github.com/.insteadOf" || env["GIT_CONFIG_VALUE_5"] != "git@github.com:" {
		t.Fatalf("expected SSH rewrite for git@github.com:, got %q=%q", env["GIT_CONFIG_KEY_5"], env["GIT_CONFIG_VALUE_5"])
	}
	if env["GIT_CONFIG_KEY_7"] != "url.https://github.com/.insteadOf" || env["GIT_CONFIG_VALUE_7"] != "ssh://git@github.com:22/" {
		t.Fatalf("expected SSH port-22 rewrite, got %q=%q", env["GIT_CONFIG_KEY_7"], env["GIT_CONFIG_VALUE_7"])
	}
}

func TestEnsureGHShim_WritesExecutableShim(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "github-broker-bin", "sess-1")
	shimPath, err := EnsureGHShim(dir, "/opt/ao/bin/ao")
	if err != nil {
		t.Fatalf("EnsureGHShim: %v", err)
	}
	data, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatalf("read shim: %v", err)
	}
	if !strings.Contains(string(data), "github-broker gh") {
		t.Fatalf("unexpected shim contents: %s", string(data))
	}
}

func TestWorkerEnv_BlocksRepoSpecificExtraHeaderInRealGitSubprocess(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}

	var mu sync.Mutex
	var receivedAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			mu.Lock()
			receivedAuth = append(receivedAuth, auth)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	parsedSrv, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	hostPort := parsedSrv.Host // e.g. 127.0.0.1:xxxxx
	repoURL := srv.URL + "/owner/repo.git"

	repoDir := t.TempDir()
	runGit := func(extraEnv map[string]string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = repoDir
		env := os.Environ()
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
		return cmd.CombinedOutput()
	}

	if out, err := runGit(nil, "init"); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	// Configure a repo-specific Authorization extraHeader that would bypass host-level resets.
	dummyAuth := "Basic ZHVtbXk6ZHVtbXk="
	if out, err := runGit(nil, "config", "--local", "http."+repoURL+".extraHeader", "Authorization: "+dummyAuth); err != nil {
		t.Fatalf("git config extraHeader: %v (%s)", err, out)
	}
	// Also configure an unrelated non-GitHub host extraHeader and verify it is NOT cleared.
	if out, err := runGit(nil, "config", "--local", "http.https://gitlab.example.com/group/proj.git.extraHeader", "Authorization: Bearer gl-dummy"); err != nil {
		t.Fatalf("git config gitlab extraHeader: %v (%s)", err, out)
	}

	keysToClear := CollectGitHubExtraHeaderKeys(context.Background(), repoDir, hostPort)
	for _, k := range keysToClear {
		if strings.Contains(strings.ToLower(k), "gitlab.example.com") {
			t.Fatalf("CollectGitHubExtraHeaderKeys unexpectedly included non-GitHub key %q", k)
		}
	}
	if len(keysToClear) == 0 {
		t.Fatalf("expected CollectGitHubExtraHeaderKeys to find repo-specific extraHeader for %s", hostPort)
	}

	workerEnv := WorkerEnv(WorkerEnvOptions{
		BrokerToken:            "sess-cap",
		GitHubHost:             hostPort,
		AOBinary:               "/nonexistent/ao",
		AuthorizedRepos:        []string{"owner/repo"},
		ExtraHeaderKeysToClear: keysToClear,
	})
	// Disable the http->https insteadOf for this local httptest.Server HTTP check
	// so git actually sends the HTTP request to srv.URL.
	for i := 0; i < 30; i++ {
		k := "GIT_CONFIG_KEY_" + string(rune('0'+i))
		if strings.HasPrefix(workerEnv[k], "url.https://") {
			workerEnv["GIT_CONFIG_VALUE_"+string(rune('0'+i))] = "ssh://unused.invalid/"
		}
	}

	// Execute real git ls-remote against the local HTTP server; it will fail with 401,
	// and must NOT send the repo-local dummy Authorization header.
	_, _ = runGit(workerEnv, "ls-remote", repoURL)

	mu.Lock()
	defer mu.Unlock()
	for _, got := range receivedAuth {
		if strings.Contains(got, "ZHVtbXk") {
			t.Fatalf("repo-specific Authorization extraHeader bypassed WorkerEnv and reached HTTP server: %q", got)
		}
	}
}

func TestWorkerEnv_BlocksRepoSpecificCredentialHelperInRealGitSubprocess(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	parsedSrv, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	hostPort := parsedSrv.Host
	repoURL := srv.URL + "/owner/repo.git"

	repoDir := t.TempDir()
	markerFile := filepath.Join(repoDir, "legacy-helper-invoked")
	runGit := func(extraEnv map[string]string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = repoDir
		env := os.Environ()
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
		return cmd.CombinedOutput()
	}

	if out, err := runGit(nil, "init"); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	// Configure a repo-specific credential helper that creates markerFile if invoked.
	if out, err := runGit(nil, "config", "--local", "credential."+repoURL+".helper", "!touch "+shellQuoteForGit(markerFile)); err != nil {
		t.Fatalf("git config credential helper: %v (%s)", err, out)
	}
	if out, err := runGit(nil, "config", "--local", "credential.https://gitlab.example.com/group/proj.git.helper", "!echo gitlab"); err != nil {
		t.Fatalf("git config gitlab helper: %v (%s)", err, out)
	}

	helperKeys := CollectGitHubCredentialHelperKeys(context.Background(), repoDir, hostPort)
	for _, k := range helperKeys {
		if strings.Contains(strings.ToLower(k), "gitlab.example.com") {
			t.Fatalf("CollectGitHubCredentialHelperKeys unexpectedly included non-GitHub key %q", k)
		}
	}
	if len(helperKeys) == 0 {
		t.Fatalf("expected CollectGitHubCredentialHelperKeys to find repo-specific helper for %s", hostPort)
	}

	workerEnv := WorkerEnv(WorkerEnvOptions{
		BrokerToken:                 "sess-cap",
		GitHubHost:                  hostPort,
		AOBinary:                    "/nonexistent/ao",
		AuthorizedRepos:             []string{"owner/repo"},
		CredentialHelperKeysToClear: helperKeys,
	})
	// Disable http->https insteadOf so git hits our local httptest.Server over HTTP.
	for i := 0; i < 40; i++ {
		k := "GIT_CONFIG_KEY_" + string(rune('0'+i))
		if strings.HasPrefix(workerEnv[k], "url.https://") {
			workerEnv["GIT_CONFIG_VALUE_"+string(rune('0'+i))] = "ssh://unused.invalid/"
		}
	}

	_, _ = runGit(workerEnv, "ls-remote", repoURL)

	if _, err := os.Stat(markerFile); err == nil {
		t.Fatalf("repo-specific legacy credential helper was invoked despite WorkerEnv override")
	}
}
