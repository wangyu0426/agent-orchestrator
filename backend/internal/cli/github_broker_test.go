package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubbroker"
)

func TestGitHubBroker_GitCredentialGetStoreErase(t *testing.T) {
	var invalidated bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cap-secret-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/internal/github/broker/token":
			var req brokerTokenRequestDTO
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.SessionID != "sess-42" || req.Repository != "wangyu0426/agent-orchestrator.git" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(brokerTokenResponseDTO{
				Token:      "ghs_cli_helper_token_1234567890",
				ExpiresAt:  time.Now().Add(1 * time.Hour),
				Username:   githubbroker.GitUsername,
				Host:       "github.com",
				Repository: "wangyu0426/agent-orchestrator",
			})
		case "/internal/github/broker/invalidate":
			invalidated = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	t.Setenv("AO_SESSION_ID", "sess-42")
	t.Setenv(githubbroker.EnvBrokerToken, "cap-secret-123")

	resp, err := callGitHubBrokerToken(t.Context(), srv.URL, srv.Client(), "github.com", "wangyu0426/agent-orchestrator.git")
	if err != nil {
		t.Fatalf("callGitHubBrokerToken: %v", err)
	}
	if resp.Token != "ghs_cli_helper_token_1234567890" || resp.Username != "x-access-token" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	if err := callGitHubBrokerInvalidate(t.Context(), srv.URL, srv.Client(), "github.com", "wangyu0426/agent-orchestrator.git"); err != nil {
		t.Fatalf("callGitHubBrokerInvalidate: %v", err)
	}
	if !invalidated {
		t.Fatalf("expected invalidate endpoint to be called")
	}

	// Verify git credential stdin parsing.
	attrs := parseGitCredentialInput(bytes.NewBufferString("protocol=https\nhost=github.com\npath=wangyu0426/agent-orchestrator.git\n\n"))
	if attrs["protocol"] != "https" || attrs["host"] != "github.com" || attrs["path"] != "wangyu0426/agent-orchestrator.git" {
		t.Fatalf("unexpected parsed git credential attrs: %+v", attrs)
	}

	// Verify gh repo flag extraction.
	if got := extractGHRepoFlag([]string{"pr", "view", "-R", "owner/repo"}); got != "owner/repo" {
		t.Fatalf("extractGHRepoFlag(-R) = %q, want owner/repo", got)
	}
	if got := extractGHRepoFlag([]string{"api", "--repo=owner/repo2"}); got != "owner/repo2" {
		t.Fatalf("extractGHRepoFlag(--repo=) = %q, want owner/repo2", got)
	}

	// Verify buildBrokerGHEnv scrubs personal tokens/GH_HOST and injects GH_ENTERPRISE_TOKEN for GHES hosts and GH_TOKEN for github.com.
	env := buildBrokerGHEnv([]string{"GITHUB_TOKEN=ghp_personal_1234567890", "GH_TOKEN=ghp_old_1234567890", "GH_HOST=evil.example.com", "PATH=/usr/bin"}, "ghs_fresh_1234567890", "ghe.internal.example", "/tmp/iso-gh")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "ghp_personal") || strings.Contains(joined, "ghp_old") || strings.Contains(joined, "evil.example.com") {
		t.Fatalf("personal tokens or inherited GH_HOST leaked into gh child env: %s", joined)
	}
	if !strings.Contains(joined, "GH_ENTERPRISE_TOKEN=ghs_fresh_1234567890") || !strings.Contains(joined, "GH_HOST=ghe.internal.example") || !strings.Contains(joined, "GH_CONFIG_DIR=/tmp/iso-gh") {
		t.Fatalf("missing broker GH_ENTERPRISE_TOKEN, GH_HOST, or GH_CONFIG_DIR in child env: %s", joined)
	}
	dotComEnv := strings.Join(buildBrokerGHEnv([]string{"PATH=/usr/bin"}, "ghs_dotcom_1234567890", "github.com", "/tmp/iso-gh"), "\n")
	if !strings.Contains(dotComEnv, "GH_TOKEN=ghs_dotcom_1234567890") || !strings.Contains(dotComEnv, "GH_HOST=github.com") {
		t.Fatalf("expected GH_TOKEN for github.com, got: %s", dotComEnv)
	}

	// Verify resolveRealGHFromPath skips broker shim dir and handles Windows PATHEXT without recursing.
	tmp := t.TempDir()
	shimDir := filepath.Join(tmp, "github-broker-bin", "sess-1")
	realDir := filepath.Join(tmp, "real-bin")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shimDir, "gh.cmd"), []byte("shim"), 0o600); err != nil {
		t.Fatal(err)
	}
	realGH := filepath.Join(realDir, "gh.exe")
	if err := os.WriteFile(realGH, []byte("real"), 0o600); err != nil {
		t.Fatal(err)
	}
	gotGH, err := resolveRealGHFromPath(shimDir+string(os.PathListSeparator)+realDir, "", "windows", ".EXE;.CMD")
	if err != nil || gotGH != realGH {
		t.Fatalf("resolveRealGHFromPath(windows) = (%q, %v), want (%q, nil)", gotGH, err, realGH)
	}
	if _, err := resolveRealGHFromPath(shimDir, "", "windows", ".EXE;.CMD"); err == nil {
		t.Fatalf("expected error when only broker shim dir is in PATH")
	}
}
