package daemon

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	scmgithub "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/github"
	trackergithub "github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/github"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubapp"
)

func writeTempAppKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	path := filepath.Join(t.TempDir(), "github-app.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write temp key: %v", err)
	}
	return path
}

func TestGitHubAppWiring_FailClosedNoFallbackToPersonalCredentials(t *testing.T) {
	t.Setenv("AO_GITHUB_TOKEN", "ghp_personal_should_never_be_used")
	t.Setenv("GITHUB_TOKEN", "ghp_personal_fallback_should_never_be_used")

	// Enabled with missing/invalid key path must fail closed.
	auth, err := newGitHubAuthWiring(config.GitHubAppConfig{
		Enabled:        true,
		AppID:          101,
		InstallationID: 202,
		PrivateKeyPath: filepath.Join(t.TempDir(), "does-not-exist.pem"),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("expected error for missing private key in app mode, got nil")
	}

	// Even if someone passes the failed githubAuthWiring to SCM or Tracker,
	// neither may fall back to AO_GITHUB_TOKEN or GITHUB_TOKEN.
	if _, scmErr := newGitHubSCMProviderWithAuth(auth, slog.New(slog.NewTextHandler(io.Discard, nil))); scmErr == nil {
		t.Fatal("expected SCM provider to fail closed in app mode without token source")
	}
	if _, trackerErr := newGitHubTrackerWithAuth(auth); trackerErr == nil {
		t.Fatal("expected Tracker to fail closed in app mode without token source")
	}
}

func TestConfig_ScrubGitHubAppEnv(t *testing.T) {
	keyPath := writeTempAppKey(t)
	t.Setenv("AO_GITHUB_APP_ENABLED", "on")
	t.Setenv("AO_GITHUB_APP_ID", "12345")
	t.Setenv("AO_GITHUB_APP_INSTALLATION_ID", "67890")
	t.Setenv("AO_GITHUB_APP_PRIVATE_KEY_PATH", keyPath)
	t.Setenv("AO_GITHUB_APP_REPOSITORIES", "agent-orchestrator,dev-rag-service")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if !cfg.GitHubApp.Enabled || cfg.GitHubApp.AppID != 12345 || cfg.GitHubApp.InstallationID != 67890 {
		t.Fatalf("unexpected GitHubApp config: %+v", cfg.GitHubApp)
	}
	if !filepath.IsAbs(cfg.GitHubApp.PrivateKeyPath) {
		t.Fatalf("expected absolute PrivateKeyPath, got %q", cfg.GitHubApp.PrivateKeyPath)
	}
	if len(cfg.GitHubApp.Repositories) != 2 {
		t.Fatalf("unexpected repositories: %v", cfg.GitHubApp.Repositories)
	}

	config.ScrubGitHubAppEnv()
	for _, k := range []string{
		"AO_GITHUB_APP_ENABLED",
		"AO_GITHUB_APP_ID",
		"AO_GITHUB_APP_INSTALLATION_ID",
		"AO_GITHUB_APP_PRIVATE_KEY_PATH",
		"AO_GITHUB_APP_REPOSITORIES",
	} {
		if v := os.Getenv(k); v != "" {
			t.Fatalf("env %s was not scrubbed: %q", k, v)
		}
	}
}

func TestGitHubApp_SCMAndTrackerIntegration(t *testing.T) {
	keyPath := writeTempAppKey(t)
	var userCalls atomic.Int32
	var installRepoCalls atomic.Int32
	var tokenExchanges atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/777/access_tokens":
			n := tokenExchanges.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      fmt.Sprintf("ghs_app_integration_token_%d", n),
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			userCalls.Add(1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			installRepoCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"total_count":1,"repositories":[{"full_name":"wangyu0426/agent-orchestrator"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/wangyu0426/agent-orchestrator/issues/59":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"number":59,"title":"GitHub App auth","state":"open","html_url":"https://github.com/wangyu0426/agent-orchestrator/issues/59"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/wangyu0426/agent-orchestrator/issues/8/comments":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"node_id":"IC_kwDO1","body":"AO status update","html_url":"https://github.com/wangyu0426/agent-orchestrator/pull/8#issuecomment-1","created_at":"2026-11-27T06:00:00Z","user":{"login":"ao-daemon[bot]","type":"Bot"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/wangyu0426/agent-orchestrator/pulls/8":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_app_integration_token_") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"number":8,"state":"open","draft":false,"merged":false,"html_url":"https://github.com/wangyu0426/agent-orchestrator/pull/8","head":{"ref":"feat/test","sha":"abc123"},"base":{"ref":"main"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_app_integration_token_") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"data": {
					"repository": {
						"pullRequest": {
							"reviewDecision": "CHANGES_REQUESTED",
							"mergeable": "MERGEABLE",
							"mergeStateStatus": "CLEAN",
							"isDraft": false,
							"state": "OPEN",
							"commits": {"nodes": [{"commit": {"oid": "abc123", "statusCheckRollup": null}}]},
							"reviewThreads": {
								"nodes": [
									{
										"id": "T_human",
										"isResolved": false,
										"path": "main.go",
										"line": 10,
										"comments": {
											"nodes": [{
												"id": "C_human",
												"body": "Please fix this before merge",
												"path": "main.go",
												"line": 10,
												"url": "https://github.com/wangyu0426/agent-orchestrator/pull/8#discussion_r1",
												"createdAt": "2026-11-27T06:01:00Z",
												"author": {"__typename": "User", "login": "wangyu0426"}
											}]
										}
									},
									{
										"id": "T_bot",
										"isResolved": false,
										"path": "",
										"line": 0,
										"comments": {
											"nodes": [{
												"id": "C_bot",
												"body": "AO status update",
												"path": "",
												"line": 0,
												"url": "https://github.com/wangyu0426/agent-orchestrator/pull/8#discussion_r2",
												"createdAt": "2026-11-27T06:02:00Z",
												"author": {"__typename": "Bot", "login": "ao-daemon[bot]"}
											}]
										}
									}
								]
							}
						}
					}
				}
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	appProvider, err := githubapp.NewProvider(githubapp.Config{
		AppID:          555,
		InstallationID: 777,
		PrivateKeyPath: keyPath,
		BaseURL:        srv.URL,
	})
	if err != nil {
		t.Fatalf("githubapp.NewProvider: %v", err)
	}
	appTokens := appProvider.TokenSource("agent-orchestrator")

	// 1. Tracker Preflight and Get work with installation token (no /user call).
	tracker, err := trackergithub.New(trackergithub.Options{
		Token:   appTokens,
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("trackergithub.New: %v", err)
	}
	if err := tracker.Preflight(context.Background()); err != nil {
		t.Fatalf("tracker.Preflight: %v", err)
	}
	issue, err := tracker.Get(context.Background(), domain.TrackerID{
		Provider: domain.TrackerProviderGitHub,
		Native:   "wangyu0426/agent-orchestrator#59",
	})
	if err != nil {
		t.Fatalf("tracker.Get: %v", err)
	}
	if issue.Title != "GitHub App auth" {
		t.Fatalf("issue.Title = %q, want GitHub App auth", issue.Title)
	}

	// 2. SCM AuthenticatedIdentity returns Human=false without calling /user.
	scmProv, err := scmgithub.NewProvider(scmgithub.ProviderOptions{
		Token:              appTokens,
		RESTBase:           srv.URL,
		GraphQLURL:         srv.URL + "/graphql",
		SkipTokenPreflight: true,
	})
	if err != nil {
		t.Fatalf("scmgithub.NewProvider: %v", err)
	}
	ident, err := scmProv.AuthenticatedIdentity(context.Background())
	if err != nil {
		t.Fatalf("AuthenticatedIdentity: %v", err)
	}
	if ident.Human {
		t.Fatalf("expected Human=false for GitHub App installation identity, got %+v", ident)
	}

	// 3. Posting a status comment returns IsBot=true (own bot comment does not trigger human feedback loop),
	// while a human comment (IsBot=false) remains actionable.
	posted, err := scmProv.PostComment(context.Background(), ports.SCMCommentRequest{
		PR:   ports.SCMPRRef{Repo: ports.SCMRepo{Provider: "github", Owner: "wangyu0426", Name: "agent-orchestrator"}, Number: 8},
		Body: "AO status update",
	})
	if err != nil {
		t.Fatalf("PostPRComment: %v", err)
	}
	if !posted.IsBot {
		t.Fatalf("expected bot-authored status comment IsBot=true, got %+v", posted)
	}

	// 4. Exercise App-authenticated REST + GraphQL PR observation through scmProv.Observe:
	// human CHANGES_REQUESTED review and unresolved human comment are observed,
	// while the App's own bot status comment is filtered out of pending review comments.
	obs, err := scmProv.Observe(context.Background(), "https://github.com/wangyu0426/agent-orchestrator/pull/8")
	if err != nil {
		t.Fatalf("scmProv.Observe: %v", err)
	}
	if !obs.Fetched {
		t.Fatal("expected obs.Fetched=true")
	}
	if obs.Review != domain.ReviewChangesRequest {
		t.Fatalf("obs.Review = %q, want %q", obs.Review, domain.ReviewChangesRequest)
	}
	if len(obs.Comments) != 1 || obs.Comments[0].Author != "wangyu0426" {
		t.Fatalf("obs.Comments = %+v, want only the 1 human change-request comment (bot status comment filtered)", obs.Comments)
	}

	if userCalls.Load() != 0 {
		t.Fatalf("expected 0 GET /user calls in GitHub App mode, got %d", userCalls.Load())
	}
	if installRepoCalls.Load() != 1 {
		t.Fatalf("expected 1 GET /installation/repositories preflight call, got %d", installRepoCalls.Load())
	}
}

func TestGitHubAppWiring_ThreadsAPIBaseURLAndGraphQL(t *testing.T) {
	if got := deriveGitHubGraphQLURL("https://github.example.com/api/v3"); got != "https://github.example.com/api/graphql" {
		t.Fatalf("GHES graphql = %q, want https://github.example.com/api/graphql", got)
	}
	if got := deriveGitHubGraphQLURL("https://api.github.com/"); got != "https://api.github.com/graphql" {
		t.Fatalf("default graphql = %q, want https://api.github.com/graphql", got)
	}

	keyPath := writeTempAppKey(t)
	var hitTracker, hitSCM atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/888/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "ghs_enterprise_token_123456",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			hitTracker.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"total_count":0,"repositories":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/org/repo/issues/1/comments":
			hitSCM.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"node_id":"IC_1","body":"ok","html_url":"https://example.com/1","created_at":"2026-11-27T06:00:00Z","user":{"login":"ao[bot]","type":"Bot"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Enabled with missing repository scope must fail closed even with a valid key.
	if _, err := newGitHubAuthWiring(config.GitHubAppConfig{
		Enabled:        true,
		AppID:          111,
		InstallationID: 888,
		PrivateKeyPath: keyPath,
		APIBaseURL:     srv.URL,
	}, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("expected error when Repositories is omitted in App mode, got nil")
	}

	auth, err := newGitHubAuthWiring(config.GitHubAppConfig{
		Enabled:        true,
		AppID:          111,
		InstallationID: 888,
		PrivateKeyPath: keyPath,
		APIBaseURL:     srv.URL,
		Repositories:   []string{"repo"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newGitHubAuthWiring: %v", err)
	}

	tracker, err := newGitHubTrackerWithAuth(auth)
	if err != nil {
		t.Fatalf("newGitHubTrackerWithAuth: %v", err)
	}
	if err := tracker.Preflight(context.Background()); err != nil {
		t.Fatalf("tracker.Preflight against custom APIBaseURL: %v", err)
	}

	scmProv, err := newGitHubSCMProviderWithAuth(auth, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newGitHubSCMProviderWithAuth: %v", err)
	}
	if _, err := scmProv.PostComment(context.Background(), ports.SCMCommentRequest{
		PR:   ports.SCMPRRef{Repo: ports.SCMRepo{Provider: "github", Owner: "org", Name: "repo"}, Number: 1},
		Body: "ok",
	}); err != nil {
		t.Fatalf("scmProv.PostComment against custom APIBaseURL: %v", err)
	}

	if hitTracker.Load() != 1 || hitSCM.Load() != 1 {
		t.Fatalf("expected custom APIBaseURL to be hit by both tracker (%d) and scm (%d)", hitTracker.Load(), hitSCM.Load())
	}

	// Even when OperatorLogin is set, SCMIdentity keeps Human=false because
	// the authenticated principal is a GitHub App installation, not a human.
	authWithOp, err := newGitHubAuthWiring(config.GitHubAppConfig{
		Enabled:        true,
		AppID:          111,
		InstallationID: 888,
		PrivateKeyPath: keyPath,
		APIBaseURL:     srv.URL,
		Repositories:   []string{"repo"},
		OperatorLogin:  "@wangyu0426",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newGitHubAuthWiring with OperatorLogin: %v", err)
	}
	scmOp, err := newGitHubSCMProviderWithAuth(authWithOp, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newGitHubSCMProviderWithAuth with OperatorLogin: %v", err)
	}
	ident, err := scmOp.AuthenticatedIdentity(context.Background())
	if err != nil {
		t.Fatalf("AuthenticatedIdentity with OperatorLogin: %v", err)
	}
	if ident.Human {
		t.Fatalf("expected Human=false for GitHub App installation principal, got %+v", ident)
	}
}
