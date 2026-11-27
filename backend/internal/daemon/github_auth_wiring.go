package daemon

import (
	"fmt"
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubapp"
)

// githubAuthWiring carries the daemon's resolved GitHub authentication mode.
// When appEnabled is true, all daemon GitHub SCM, Tracker, and resolver paths
// use appTokens exclusively and fail closed (never falling back to PAT/env/gh).
type githubAuthWiring struct {
	appEnabled bool
	appTokens  *githubapp.ScopedTokenSource
}

func newGitHubAuthWiring(cfg config.GitHubAppConfig, logger *slog.Logger) (githubAuthWiring, error) {
	if !cfg.Enabled {
		return githubAuthWiring{}, nil
	}
	provider, err := githubapp.NewProvider(githubapp.Config{
		AppID:          cfg.AppID,
		InstallationID: cfg.InstallationID,
		PrivateKeyPath: cfg.PrivateKeyPath,
		BaseURL:        cfg.APIBaseURL,
		Logger:         logger,
	})
	if err != nil {
		return githubAuthWiring{appEnabled: true}, fmt.Errorf("github app auth: %w", err)
	}
	return githubAuthWiring{
		appEnabled: true,
		appTokens:  provider.TokenSource(cfg.Repositories...),
	}, nil
}
