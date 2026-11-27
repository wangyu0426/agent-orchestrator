package daemon

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubapp"
)

// githubAuthWiring carries the daemon's resolved GitHub authentication mode.
// When appEnabled is true, all daemon GitHub SCM, Tracker, and resolver paths
// use appTokens exclusively and fail closed (never falling back to PAT/env/gh).
type githubAuthWiring struct {
	appEnabled bool
	appTokens  *githubapp.ScopedTokenSource
	apiBaseURL string
	graphqlURL string
}

func newGitHubAuthWiring(cfg config.GitHubAppConfig, logger *slog.Logger) (githubAuthWiring, error) {
	if !cfg.Enabled {
		return githubAuthWiring{}, nil
	}
	repos, err := githubapp.ValidateRepositories(cfg.Repositories)
	if err != nil {
		return githubAuthWiring{appEnabled: true}, fmt.Errorf("github app auth: %w", err)
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.APIBaseURL), "/")
	provider, err := githubapp.NewProvider(githubapp.Config{
		AppID:          cfg.AppID,
		InstallationID: cfg.InstallationID,
		PrivateKeyPath: cfg.PrivateKeyPath,
		BaseURL:        baseURL,
		Logger:         logger,
	})
	if err != nil {
		return githubAuthWiring{appEnabled: true}, fmt.Errorf("github app auth: %w", err)
	}
	return githubAuthWiring{
		appEnabled: true,
		appTokens:  provider.TokenSource(repos...),
		apiBaseURL: baseURL,
		graphqlURL: deriveGitHubGraphQLURL(baseURL),
	}, nil
}

func deriveGitHubGraphQLURL(restBase string) string {
	restBase = strings.TrimRight(strings.TrimSpace(restBase), "/")
	if restBase == "" {
		return ""
	}
	if prefix, ok := strings.CutSuffix(restBase, "/api/v3"); ok {
		return prefix + "/api/graphql"
	}
	return restBase + "/graphql"
}
