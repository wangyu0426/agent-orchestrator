package githubbroker

import (
	"os"
	"path/filepath"
	"strings"
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
		if v, ok := env[k]; !ok || v != "" {
			t.Fatalf("expected inherited credential %s to be scrubbed to empty, got %q (present=%v)", k, v, ok)
		}
	}
	if env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("expected GIT_TERMINAL_PROMPT=0, got %q", env["GIT_TERMINAL_PROMPT"])
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
	if env["GIT_CONFIG_KEY_0"] != "credential.helper" || env["GIT_CONFIG_VALUE_0"] != "" {
		t.Fatalf("expected first git config to clear credential.helper, got %q=%q", env["GIT_CONFIG_KEY_0"], env["GIT_CONFIG_VALUE_0"])
	}
	if !strings.Contains(env["GIT_CONFIG_VALUE_1"], "github-broker git-credential") {
		t.Fatalf("expected broker credential helper, got %q", env["GIT_CONFIG_VALUE_1"])
	}
	if env["GIT_CONFIG_KEY_2"] != "credential.useHttpPath" || env["GIT_CONFIG_VALUE_2"] != "true" {
		t.Fatalf("expected credential.useHttpPath=true, got %q=%q", env["GIT_CONFIG_KEY_2"], env["GIT_CONFIG_VALUE_2"])
	}
	if env["GIT_CONFIG_KEY_3"] != "url.https://github.com/.insteadOf" || env["GIT_CONFIG_VALUE_3"] != "git@github.com:" {
		t.Fatalf("expected SSH rewrite for git@github.com:, got %q=%q", env["GIT_CONFIG_KEY_3"], env["GIT_CONFIG_VALUE_3"])
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
