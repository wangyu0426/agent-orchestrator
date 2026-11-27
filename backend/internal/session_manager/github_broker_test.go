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

	// Non-GitHub (GitLab / local-only) projects skip broker registration without error.
	store.projects["proj-gitlab"] = domain.ProjectRecord{
		ID:            "proj-gitlab",
		RepoOriginURL: "https://gitlab.com/group/project.git",
	}
	glEnv, _, err := mgr.launchRuntimeEnvForHarness(context.Background(), "sess-gitlab", "proj-gitlab", "", nil, domain.AgentHarness("codex"))
	if err != nil {
		t.Fatalf("expected non-GitHub project to skip broker, got %v", err)
	}
	if glEnv[githubbroker.EnvBrokerToken] != "" {
		t.Fatalf("expected no broker token for non-GitHub project, got %q", glEnv[githubbroker.EnvBrokerToken])
	}

	// GitHub SSH URL with explicit port is recognized and brokered.
	store.projects["proj-ssh-port"] = domain.ProjectRecord{
		ID:            "proj-ssh-port",
		RepoOriginURL: "ssh://git@github.com:22/wangyu0426/agent-orchestrator.git",
	}
	sshEnv, _, err := mgr.launchRuntimeEnvForHarness(context.Background(), "sess-ssh-port", "proj-ssh-port", "", nil, domain.AgentHarness("codex"))
	if err != nil {
		t.Fatalf("expected ssh://git@github.com:22 origin to be brokered, got %v", err)
	}
	if sshEnv[githubbroker.EnvBrokerToken] == "" {
		t.Fatalf("expected broker token for ssh://git@github.com:22 origin")
	}

	// Malformed GitHub origin fails closed instead of falling back to legacy credentials.
	store.projects["proj-bad-gh"] = domain.ProjectRecord{
		ID:            "proj-bad-gh",
		RepoOriginURL: "https://github.com/only-one-segment",
	}
	if _, _, err := mgr.launchRuntimeEnvForHarness(context.Background(), "sess-bad-gh", "proj-bad-gh", "", nil, domain.AgentHarness("codex")); !errors.Is(err, githubbroker.ErrForbiddenRepo) {
		t.Fatalf("expected ErrForbiddenRepo for malformed GitHub origin, got %v", err)
	}

	// GitHub HTTPS origin with embedded userinfo fails closed.
	store.projects["proj-userinfo-gh"] = domain.ProjectRecord{
		ID:            "proj-userinfo-gh",
		RepoOriginURL: "https://x-access-token:ghp_secret123@github.com/wangyu0426/agent-orchestrator.git",
	}
	if _, _, err := mgr.launchRuntimeEnvForHarness(context.Background(), "sess-userinfo-gh", "proj-userinfo-gh", "", nil, domain.AgentHarness("codex")); !errors.Is(err, githubbroker.ErrForbiddenRepo) {
		t.Fatalf("expected ErrForbiddenRepo for GitHub origin with embedded credentials, got %v", err)
	}

	// Workspace project with empty parent origin binds child GitHub repos.
	store.projects["proj-ws"] = domain.ProjectRecord{
		ID:   "proj-ws",
		Kind: domain.ProjectKindWorkspace,
	}
	store.workspaceRepo["proj-ws"] = []domain.WorkspaceRepoRecord{
		{ProjectID: "proj-ws", Name: "agent-orchestrator", RepoOriginURL: "https://github.com/wangyu0426/agent-orchestrator.git"},
	}
	wsEnv, _, err := mgr.launchRuntimeEnvForHarness(context.Background(), "sess-ws", "proj-ws", "", nil, domain.AgentHarness("codex"))
	if err != nil {
		t.Fatalf("expected workspace project with allowlisted child GitHub repo to succeed, got %v", err)
	}
	if wsEnv[githubbroker.EnvBrokerToken] == "" {
		t.Fatalf("expected broker token for workspace project with child GitHub repo")
	}

	// Workspace project with an unlisted child GitHub repo fails closed.
	store.projects["proj-ws-unlisted"] = domain.ProjectRecord{
		ID:   "proj-ws-unlisted",
		Kind: domain.ProjectKindWorkspace,
	}
	store.workspaceRepo["proj-ws-unlisted"] = []domain.WorkspaceRepoRecord{
		{ProjectID: "proj-ws-unlisted", Name: "secret", RepoOriginURL: "https://github.com/other-org/secret.git"},
	}
	if _, _, err := mgr.launchRuntimeEnvForHarness(context.Background(), "sess-ws-unlisted", "proj-ws-unlisted", "", nil, domain.AgentHarness("codex")); !errors.Is(err, githubbroker.ErrForbiddenRepo) {
		t.Fatalf("expected ErrForbiddenRepo for workspace project with unlisted child GitHub repo, got %v", err)
	}

	// GitHub SSH URL with unsupported non-22 port fails closed.
	store.projects["proj-ssh-2222"] = domain.ProjectRecord{
		ID:            "proj-ssh-2222",
		RepoOriginURL: "ssh://git@github.com:2222/wangyu0426/agent-orchestrator.git",
	}
	if _, _, err := mgr.launchRuntimeEnvForHarness(context.Background(), "sess-ssh-2222", "proj-ssh-2222", "", nil, domain.AgentHarness("codex")); !errors.Is(err, githubbroker.ErrForbiddenRepo) {
		t.Fatalf("expected ErrForbiddenRepo for unsupported SSH port 2222, got %v", err)
	}

	// Noncanonical uppercase SSH host fails closed so Git's literal insteadOf cannot be bypassed.
	store.projects["proj-ssh-upper"] = domain.ProjectRecord{
		ID:            "proj-ssh-upper",
		RepoOriginURL: "git@GITHUB.COM:wangyu0426/agent-orchestrator.git",
	}
	if _, _, err := mgr.launchRuntimeEnvForHarness(context.Background(), "sess-ssh-upper", "proj-ssh-upper", "", nil, domain.AgentHarness("codex")); !errors.Is(err, githubbroker.ErrForbiddenRepo) {
		t.Fatalf("expected ErrForbiddenRepo for noncanonical uppercase SSH host, got %v", err)
	}

	// GitHub project outside allowlist still fails closed.
	store.projects["proj-unlisted"] = domain.ProjectRecord{
		ID:            "proj-unlisted",
		RepoOriginURL: "https://github.com/other-org/secret-repo.git",
	}
	if _, _, err := mgr.launchRuntimeEnvForHarness(context.Background(), "sess-unlisted", "proj-unlisted", "", nil, domain.AgentHarness("codex")); !errors.Is(err, githubbroker.ErrForbiddenRepo) {
		t.Fatalf("expected ErrForbiddenRepo for unlisted GitHub repo, got %v", err)
	}
}
