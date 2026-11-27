package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/process"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubapp"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubbroker"
)

type brokerTokenRequestDTO struct {
	SessionID  string `json:"session_id"`
	Host       string `json:"host,omitempty"`
	Repository string `json:"repository,omitempty"`
}

type brokerTokenResponseDTO struct {
	Token      string    `json:"token"`
	ExpiresAt  time.Time `json:"expires_at"`
	Username   string    `json:"username"`
	Host       string    `json:"host"`
	Repository string    `json:"repository"`
}

func newGitHubBrokerCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "github-broker",
		Short:  "Internal GitHub App credential broker helpers for workers",
		Hidden: true,
	}
	cmd.AddCommand(newGitHubBrokerGitCredentialCommand(ctx))
	cmd.AddCommand(newGitHubBrokerGHCommand(ctx))
	return cmd
}

func newGitHubBrokerGitCredentialCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "git-credential <get|store|erase>",
		Short: "Git credential helper backed by the AO GitHub App broker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			op := strings.ToLower(strings.TrimSpace(args[0]))
			switch op {
			case "store":
				// Never persist installation tokens to disk, keychain, or git config.
				return nil
			case "get", "erase":
				attrs := parseGitCredentialInput(cmd.InOrStdin())
				if proto := strings.ToLower(attrs["protocol"]); proto != "" && proto != "https" {
					return fmt.Errorf("github-broker: unsupported git credential protocol %q", proto)
				}
				if op == "erase" {
					return ctx.invalidateGitHubBrokerToken(cmd.Context(), attrs["host"], attrs["path"])
				}
				resp, err := ctx.fetchGitHubBrokerToken(cmd.Context(), attrs["host"], attrs["path"])
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				_, _ = fmt.Fprintf(out, "protocol=https\nhost=%s\nusername=%s\npassword=%s\n\n", resp.Host, resp.Username, resp.Token)
				return nil
			default:
				return usageError{err: fmt.Errorf("unknown git-credential operation %q (expected get, store, or erase)", op)}
			}
		},
	}
}

func newGitHubBrokerGHCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:                "gh [args...]",
		Short:              "Invoke gh with an on-demand repo-scoped GitHub App token",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && args[0] == "--" {
				args = args[1:]
			}
			repo := extractGHRepoFlag(args)
			host := strings.TrimSpace(os.Getenv(githubbroker.EnvBrokerHost))
			resp, err := ctx.fetchGitHubBrokerToken(cmd.Context(), host, repo)
			if err != nil {
				return err
			}

			realGH, err := resolveRealGHBinary()
			if err != nil {
				return fmt.Errorf("github-broker: %w", err)
			}

			ghConfigParent, err := resolveBrokerGHConfigParent()
			if err != nil {
				return fmt.Errorf("github-broker: resolve gh config dir: %w", err)
			}
			isolatedConfigDir, err := os.MkdirTemp(ghConfigParent, "gh-*")
			if err != nil {
				return fmt.Errorf("github-broker: create isolated gh config dir: %w", err)
			}
			defer func() { _ = os.RemoveAll(isolatedConfigDir) }()

			child := process.CommandContext(cmd.Context(), realGH, args...)
			child.Stdin = cmd.InOrStdin()
			child.Stdout = cmd.OutOrStdout()
			child.Stderr = cmd.ErrOrStderr()
			child.Env = buildBrokerGHEnv(os.Environ(), resp.Token, resp.Host, isolatedConfigDir)

			if err := child.Run(); err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					return fmt.Errorf("github-broker: gh exited with status %d", exitErr.ExitCode())
				}
				return fmt.Errorf("github-broker: run gh: %v", githubapp.Redact(err.Error()))
			}
			return nil
		},
	}
}

func parseGitCredentialInput(r io.Reader) map[string]string {
	attrs := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		attrs[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return attrs
}

func (c *commandContext) fetchGitHubBrokerToken(ctx context.Context, host, repo string) (brokerTokenResponseDTO, error) {
	sessionID := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
	brokerToken := strings.TrimSpace(os.Getenv(githubbroker.EnvBrokerToken))
	if sessionID == "" || brokerToken == "" {
		return brokerTokenResponseDTO{}, errors.New("github-broker: missing AO_SESSION_ID or AO_GITHUB_BROKER_TOKEN")
	}
	var resp brokerTokenResponseDTO
	err := c.doJSONPathWithHeaders(ctx, http.MethodPost, "/internal/github/broker/token", brokerTokenRequestDTO{
		SessionID:  sessionID,
		Host:       host,
		Repository: repo,
	}, &resp, map[string]string{
		"Authorization": "Bearer " + brokerToken,
	})
	if err != nil {
		return brokerTokenResponseDTO{}, fmt.Errorf("github-broker: %v", githubapp.Redact(err.Error()))
	}
	if resp.Token == "" {
		return brokerTokenResponseDTO{}, errors.New("github-broker: empty token returned by daemon")
	}
	return resp, nil
}

func (c *commandContext) invalidateGitHubBrokerToken(ctx context.Context, host, repo string) error {
	sessionID := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
	brokerToken := strings.TrimSpace(os.Getenv(githubbroker.EnvBrokerToken))
	if sessionID == "" || brokerToken == "" {
		return nil
	}
	_ = c.doJSONPathWithHeaders(ctx, http.MethodPost, "/internal/github/broker/invalidate", brokerTokenRequestDTO{
		SessionID:  sessionID,
		Host:       host,
		Repository: repo,
	}, nil, map[string]string{
		"Authorization": "Bearer " + brokerToken,
	})
	return nil
}

func callGitHubBrokerToken(ctx context.Context, baseURL string, httpClient *http.Client, host, repo string) (brokerTokenResponseDTO, error) {
	sessionID := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
	brokerToken := strings.TrimSpace(os.Getenv(githubbroker.EnvBrokerToken))
	if sessionID == "" || brokerToken == "" {
		return brokerTokenResponseDTO{}, errors.New("github-broker: missing AO_SESSION_ID or AO_GITHUB_BROKER_TOKEN")
	}
	payload, err := json.Marshal(brokerTokenRequestDTO{
		SessionID:  sessionID,
		Host:       host,
		Repository: repo,
	})
	if err != nil {
		return brokerTokenResponseDTO{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/internal/github/broker/token", bytes.NewReader(payload))
	if err != nil {
		return brokerTokenResponseDTO{}, err
	}
	req.Header.Set("Authorization", "Bearer "+brokerToken)
	req.Header.Set("Content-Type", "application/json")

	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return brokerTokenResponseDTO{}, fmt.Errorf("github-broker: daemon request failed: %v", githubapp.Redact(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return brokerTokenResponseDTO{}, fmt.Errorf("github-broker: read response: %v", githubapp.Redact(err.Error()))
	}
	if resp.StatusCode != http.StatusOK {
		return brokerTokenResponseDTO{}, fmt.Errorf("github-broker: broker returned %d: %s", resp.StatusCode, githubapp.Redact(strings.TrimSpace(string(body))))
	}
	var parsed brokerTokenResponseDTO
	if err := json.Unmarshal(body, &parsed); err != nil {
		return brokerTokenResponseDTO{}, fmt.Errorf("github-broker: decode response: %v", githubapp.Redact(err.Error()))
	}
	if parsed.Token == "" {
		return brokerTokenResponseDTO{}, errors.New("github-broker: empty token returned by daemon")
	}
	return parsed, nil
}

func callGitHubBrokerInvalidate(ctx context.Context, baseURL string, httpClient *http.Client, host, repo string) error {
	sessionID := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
	brokerToken := strings.TrimSpace(os.Getenv(githubbroker.EnvBrokerToken))
	if sessionID == "" || brokerToken == "" {
		return nil
	}
	payload, err := json.Marshal(brokerTokenRequestDTO{
		SessionID:  sessionID,
		Host:       host,
		Repository: repo,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/internal/github/broker/invalidate", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+brokerToken)
	req.Header.Set("Content-Type", "application/json")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("github-broker: invalidate request failed: %v", githubapp.Redact(err.Error()))
	}
	_ = resp.Body.Close()
	return nil
}

func extractGHRepoFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-R" || arg == "--repo" {
			if i+1 < len(args) {
				return strings.TrimSpace(args[i+1])
			}
		}
		if v, ok := strings.CutPrefix(arg, "--repo="); ok {
			return strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(arg, "-R="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func resolveRealGHBinary() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv(githubbroker.EnvRealGH)); explicit != "" {
		return explicit, nil
	}
	return resolveRealGHFromPath(os.Getenv("PATH"), runtime.GOOS, os.Getenv("PATHEXT"))
}

func resolveRealGHFromPath(pathEnv, goos, pathExt string) (string, error) {
	candidates := []string{"gh"}
	if goos == "windows" {
		exts := []string{".exe", ".cmd", ".bat", ".com"}
		if strings.TrimSpace(pathExt) != "" {
			var parsed []string
			for _, ext := range strings.Split(pathExt, ";") {
				ext = strings.ToLower(strings.TrimSpace(ext))
				if ext != "" && strings.HasPrefix(ext, ".") {
					parsed = append(parsed, ext)
				}
			}
			if len(parsed) > 0 {
				exts = parsed
			}
		}
		candidates = make([]string, 0, len(exts))
		for _, ext := range exts {
			candidates = append(candidates, "gh"+ext)
		}
	}

	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		clean := filepath.Clean(dir)
		if strings.Contains(clean, "github-broker-bin") {
			continue
		}
		for _, name := range candidates {
			candidate := filepath.Join(dir, name)
			info, err := os.Stat(candidate)
			if err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("gh executable not found in PATH outside broker shim: %w", exec.ErrNotFound)
}

func buildBrokerGHEnv(baseEnv []string, token, host, configDir string) []string {
	scrub := map[string]struct{}{
		"GH_TOKEN":                {},
		"GITHUB_TOKEN":            {},
		"AO_GITHUB_TOKEN":         {},
		"GH_ENTERPRISE_TOKEN":     {},
		"GITHUB_ENTERPRISE_TOKEN": {},
		"GH_CONFIG_DIR":           {},
		"GH_HOST":                 {},
	}
	out := make([]string, 0, len(baseEnv)+3)
	for _, kv := range baseEnv {
		k, _, _ := strings.Cut(kv, "=")
		if _, skip := scrub[k]; skip {
			continue
		}
		out = append(out, kv)
	}
	h := strings.ToLower(strings.TrimSpace(host))
	tokenVar := "GH_TOKEN"
	if isEnterpriseGHESHost(h) {
		tokenVar = "GH_ENTERPRISE_TOKEN"
	}
	out = append(out, tokenVar+"="+token, "GH_CONFIG_DIR="+configDir)
	if h != "" {
		out = append(out, "GH_HOST="+h)
	}
	return out
}

func isEnterpriseGHESHost(host string) bool {
	if host == "" || host == "github.com" || host == "ghe.com" || strings.HasSuffix(host, ".ghe.com") || host == "github.localhost" {
		return false
	}
	return true
}

func resolveBrokerGHConfigParent() (string, error) {
	dataDir := strings.TrimSpace(os.Getenv("AO_DATA_DIR"))
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataDir = filepath.Join(home, ".ao")
	}
	parent := filepath.Join(dataDir, "runtime", "github-broker-gh-tmp")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	return parent, nil
}
