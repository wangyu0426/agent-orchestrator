package sessionmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubapp"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubbroker"
)

type stubBrokerMinter struct{}

func (stubBrokerMinter) MintScopedToken(_ context.Context, req githubapp.ScopedTokenRequest) (githubapp.ScopedToken, error) {
	return githubapp.ScopedToken{
		Token:        "ghs_session_mgr_token_1234567890",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
		Repositories: req.Repositories,
		FullNames:    []string{"wangyu0426/agent-orchestrator"},
		Permissions:  req.Permissions,
	}, nil
}

func (stubBrokerMinter) InvalidateScopedToken(_ githubapp.ScopedTokenRequest) {}

func TestManager_GitHubBrokerLifecycleWiring(t *testing.T) {
	broker, err := githubbroker.New(githubbroker.Config{
		Provider:     stubBrokerMinter{},
		AllowedRepos: []string{"wangyu0426/agent-orchestrator"},
	})
	if err != nil {
		t.Fatalf("githubbroker.New: %v", err)
	}

	store := newFakeStore()
	store.projects["proj-1"] = domain.ProjectRecord{
		ID:            "proj-1",
		RepoOriginURL: "https://github.com/wangyu0426/agent-orchestrator.git",
	}
	dataDir := t.TempDir()
	mgr := &Manager{
		store:        store,
		dataDir:      dataDir,
		githubBroker: broker,
		executable:   func() (string, error) { return "/usr/local/bin/ao", nil },
	}

	env, _, err := mgr.launchRuntimeEnvForHarness(
		context.Background(),
		"sess-broker-1",
		"proj-1",
		"",
		map[string]string{"GH_TOKEN": "ghp_personal_should_be_scrubbed"},
		domain.AgentHarness("codex"),
	)
	if err != nil {
		t.Fatalf("launchRuntimeEnvForHarness: %v", err)
	}

	tok := env[githubbroker.EnvBrokerToken]
	if tok == "" {
		t.Fatalf("expected %s in worker env", githubbroker.EnvBrokerToken)
	}
	if env["GH_TOKEN"] != "" {
		t.Fatalf("expected GH_TOKEN to be scrubbed, got %q", env["GH_TOKEN"])
	}
	if !strings.Contains(env["GIT_CONFIG_VALUE_1"], "github-broker git-credential") {
		t.Fatalf("expected git credential helper in env, got %q", env["GIT_CONFIG_VALUE_1"])
	}
	shimDir := filepath.Join(dataDir, "runtime", "github-broker-bin", "sess-broker-1")
	if _, err := os.Stat(shimDir); err != nil {
		t.Fatalf("expected gh shim dir %s to exist: %v", shimDir, err)
	}

	// Broker should authorize this session for its registered repo and reject others.
	resp, err := broker.IssueToken(context.Background(), githubbroker.IssueRequest{
		SessionID:   "sess-broker-1",
		BrokerToken: tok,
		Repository:  "wangyu0426/agent-orchestrator",
	})
	if err != nil || resp.Token != "ghs_session_mgr_token_1234567890" {
		t.Fatalf("IssueToken failed: resp=%+v err=%v", resp, err)
	}
	if _, err := broker.IssueToken(context.Background(), githubbroker.IssueRequest{
		SessionID:   "sess-broker-1",
		BrokerToken: tok,
		Repository:  "other/repo",
	}); !errors.Is(err, githubbroker.ErrForbiddenRepo) {
		t.Fatalf("expected ErrForbiddenRepo for other repo, got %v", err)
	}

	// Teardown revokes the session credential and removes shim/config dirs.
	mgr.revokeGitHubBrokerSession("sess-broker-1")
	if _, err := broker.IssueToken(context.Background(), githubbroker.IssueRequest{
		SessionID:   "sess-broker-1",
		BrokerToken: tok,
		Repository:  "wangyu0426/agent-orchestrator",
	}); !errors.Is(err, githubbroker.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized after revoke, got %v", err)
	}
	if _, err := os.Stat(shimDir); !os.IsNotExist(err) {
		t.Fatalf("expected gh shim dir to be removed on teardown, err=%v", err)
	}
}
