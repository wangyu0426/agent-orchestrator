package githubbroker

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/process"
)

const (
	// EnvBrokerToken is the per-session bearer capability injected into workers.
	EnvBrokerToken = "AO_GITHUB_BROKER_TOKEN" //nolint:gosec // Environment variable name, not a credential.
	// EnvBrokerHost is the GitHub host served by the credential broker.
	EnvBrokerHost = "AO_GITHUB_BROKER_HOST"
	// EnvRealGH optionally pins the real gh binary path for the gh shim.
	EnvRealGH = "AO_GITHUB_BROKER_REAL_GH"
)

// InheritedCredentialEnvKeys are personal GitHub/git credential env vars
// scrubbed from worker environments when the GitHub App broker is active.
var InheritedCredentialEnvKeys = []string{
	"GH_TOKEN",
	"GITHUB_TOKEN",
	"AO_GITHUB_TOKEN",
	"GH_ENTERPRISE_TOKEN",
	"GITHUB_ENTERPRISE_TOKEN",
	"GIT_ASKPASS",
	"SSH_ASKPASS",
	"SSH_AUTH_SOCK",
	"GIT_SSH",
	"GIT_SSH_COMMAND",
	"GIT_CONFIG_PARAMETERS",
}

// WorkerEnvOptions configures the worker environment overlays for broker mode.
type WorkerEnvOptions struct {
	BrokerToken            string
	GitHubHost             string
	AOBinary               string
	GHConfigDir            string
	GHShimDir              string
	CurrentPATH            string
	AuthorizedRepos        []string
	ExtraHeaderKeysToClear []string
}

// WorkerEnv returns environment variables that bind a worker process to the
// GitHub App credential broker:
//   - scrubs inherited personal GitHub tokens and askpass hooks
//   - forces Git HTTPS auth through `ao github-broker git-credential` (clearing
//     any system/global credential.helper to prevent helper bypasses)
//   - enables credential.useHttpPath so Git sends the repo path to the broker
//   - rewrites SSH GitHub remotes to HTTPS so personal SSH keys cannot bypass
//     the broker
//   - isolates GH_CONFIG_DIR so global ~/.config/gh is never read or mutated
//   - prepends GHShimDir to PATH when provided so `gh` invocations fetch fresh
//     tokens on demand
//
// Commit author metadata (GIT_AUTHOR_NAME, GIT_AUTHOR_EMAIL, etc.) is
// intentionally left untouched so commit authorship stays distinct from the
// authenticated GitHub App push actor.
func WorkerEnv(opts WorkerEnvOptions) map[string]string {
	host := strings.ToLower(strings.TrimSpace(opts.GitHubHost))
	if host == "" {
		host = DefaultGitHubHost
	}
	aoBin := strings.TrimSpace(opts.AOBinary)
	if aoBin == "" {
		aoBin = "ao"
	}

	env := make(map[string]string, 24)
	for _, k := range InheritedCredentialEnvKeys {
		env[k] = ""
	}
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GIT_SSH_COMMAND"] = fmt.Sprintf("%s github-broker ssh-guard", shellQuoteForGit(aoBin))
	env[EnvBrokerToken] = opts.BrokerToken
	env[EnvBrokerHost] = host

	if opts.GHConfigDir != "" {
		env["GH_CONFIG_DIR"] = opts.GHConfigDir
	}

	// Environment-only Git config (never mutates ~/.gitconfig or .git/config).
	// Scope credential helper and extraHeader overrides to the configured GitHub
	// host so non-GitHub providers (such as GitLab) remain usable.
	helperCmd := fmt.Sprintf("!%s github-broker git-credential", shellQuoteForGit(aoBin))
	gitConfigs := [][2]string{
		{fmt.Sprintf("credential.https://%s.helper", host), ""},
		{fmt.Sprintf("credential.https://%s.helper", host), helperCmd},
		{fmt.Sprintf("credential.https://%s.useHttpPath", host), "true"},
		{fmt.Sprintf("http.https://%s/.extraHeader", host), ""},
		{fmt.Sprintf("http.https://%s.extraHeader", host), ""},
		{fmt.Sprintf("url.https://%s/.insteadOf", host), fmt.Sprintf("git@%s:", host)},
		{fmt.Sprintf("url.https://%s/.insteadOf", host), fmt.Sprintf("ssh://git@%s/", host)},
		{fmt.Sprintf("url.https://%s/.insteadOf", host), fmt.Sprintf("ssh://git@%s:22/", host)},
		{fmt.Sprintf("url.https://%s/.insteadOf", host), fmt.Sprintf("http://%s/", host)},
		{fmt.Sprintf("credential.http://%s.helper", host), ""},
		{fmt.Sprintf("credential.http://%s.helper", host), helperCmd},
		{fmt.Sprintf("http.http://%s/.extraHeader", host), ""},
		{fmt.Sprintf("http.http://%s.extraHeader", host), ""},
	}
	seenKeys := make(map[string]struct{}, len(gitConfigs)+len(opts.AuthorizedRepos)*4+len(opts.ExtraHeaderKeysToClear))
	for _, kv := range gitConfigs {
		seenKeys[strings.ToLower(kv[0])] = struct{}{}
	}
	addResetKey := func(key string) {
		k := strings.TrimSpace(key)
		if k == "" {
			return
		}
		lk := strings.ToLower(k)
		if _, exists := seenKeys[lk]; exists {
			return
		}
		seenKeys[lk] = struct{}{}
		gitConfigs = append(gitConfigs, [2]string{k, ""})
	}
	for _, rawRepo := range opts.AuthorizedRepos {
		full, _, err := parseOwnerRepo(rawRepo)
		if err != nil {
			continue
		}
		for _, scheme := range []string{"https", "http"} {
			addResetKey(fmt.Sprintf("http.%s://%s/%s.extraHeader", scheme, host, full))
			addResetKey(fmt.Sprintf("http.%s://%s/%s.git.extraHeader", scheme, host, full))
			addResetKey(fmt.Sprintf("http.%s://%s/%s/.extraHeader", scheme, host, full))
			addResetKey(fmt.Sprintf("http.%s://%s/%s.git/.extraHeader", scheme, host, full))
		}
	}
	for _, key := range opts.ExtraHeaderKeysToClear {
		addResetKey(key)
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(len(gitConfigs))
	for i, kv := range gitConfigs {
		env[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)] = kv[0]
		env[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)] = kv[1]
	}

	if opts.GHShimDir != "" {
		if opts.CurrentPATH != "" {
			env["PATH"] = opts.GHShimDir + string(os.PathListSeparator) + opts.CurrentPATH
		} else {
			env["PATH"] = opts.GHShimDir
		}
	}
	return env
}

// EnsureGHShim writes a thin `gh` executable shim into dir that delegates every
// `gh` invocation to `<aoBinary> github-broker gh -- "$@"`.
func EnsureGHShim(dir, aoBinary string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create gh shim dir: %w", err)
	}
	if runtime.GOOS == "windows" {
		shimPath := filepath.Join(dir, "gh.cmd")
		content := fmt.Sprintf("@echo off\r\n\"%s\" github-broker gh -- %%*\r\n", aoBinary)
		if err := os.WriteFile(shimPath, []byte(content), 0o600); err != nil {
			return "", fmt.Errorf("write gh shim: %w", err)
		}
		if err := os.Chmod(shimPath, 0o700); err != nil { //nolint:gosec // Executable gh wrapper shim must be owner-executable.
			return "", fmt.Errorf("chmod gh shim: %w", err)
		}
		return shimPath, nil
	}
	shimPath := filepath.Join(dir, "gh")
	content := fmt.Sprintf("#!/bin/sh\nexec %s github-broker gh -- \"$@\"\n", shellQuoteForGit(aoBinary))
	if err := os.WriteFile(shimPath, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("write gh shim: %w", err)
	}
	if err := os.Chmod(shimPath, 0o700); err != nil { //nolint:gosec // Executable gh wrapper shim must be owner-executable.
		return "", fmt.Errorf("chmod gh shim: %w", err)
	}
	return shimPath, nil
}

func shellQuoteForGit(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CollectGitHubExtraHeaderKeys inspects effective Git config (system, global,
// and repository-local when repoDir is non-empty) and returns every
// `http.<url>.extraheader` key whose URL targets host (or bare `http.extraheader`
// when host matches). Non-GitHub hosts (such as GitLab) are untouched.
func CollectGitHubExtraHeaderKeys(ctx context.Context, repoDir, host string) []string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		h = DefaultGitHubHost
	}
	args := []string{"config", "--name-only", "--get-regexp", `^http\..*\.extraheader$`}
	if strings.TrimSpace(repoDir) != "" {
		args = append([]string{"-C", repoDir}, args...)
	}
	out, err := process.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var keys []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		key := strings.TrimSpace(scanner.Text())
		if key == "" {
			continue
		}
		if !extraHeaderKeyTargetsHost(key, h) {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func extraHeaderKeyTargetsHost(key, host string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if !strings.HasPrefix(lower, "http.") || !strings.HasSuffix(lower, ".extraheader") {
		return false
	}
	middle := key[len("http.") : len(key)-len(".extraheader")]
	u, err := url.Parse(middle)
	if err != nil {
		return false
	}
	uHost := strings.ToLower(strings.TrimSpace(u.Hostname()))
	uHostPort := strings.ToLower(strings.TrimSpace(u.Host))
	return uHost == host || uHostPort == host
}
