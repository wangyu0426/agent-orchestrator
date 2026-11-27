package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubapp"
)

// githubAuthWiring carries the daemon's resolved GitHub authentication mode.
// When appEnabled is true, all daemon GitHub SCM, Tracker, and resolver paths
// use appTokens exclusively and fail closed (never falling back to PAT/env/gh).
type githubAuthWiring struct {
	appEnabled    bool
	appTokens     *githubapp.ScopedTokenSource
	apiBaseURL    string
	graphqlURL    string
	operatorLogin string
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
		appEnabled:    true,
		appTokens:     provider.TokenSource(repos...),
		apiBaseURL:    baseURL,
		graphqlURL:    deriveGitHubGraphQLURL(baseURL),
		operatorLogin: strings.TrimSpace(strings.TrimPrefix(cfg.OperatorLogin, "@")),
	}, nil
}

// attributionResolver returns a ScopedIdentityResolver for PR attribution
// consumers (automatic PR discovery and branch PR claiming). When App mode has
// an explicit OperatorLogin configured, it supplies that operator identity for
// GitHub attribution checks without ever misrepresenting the App installation
// token itself as a human authenticated principal.
func (w githubAuthWiring) attributionResolver(base ports.ScopedIdentityResolver) ports.ScopedIdentityResolver {
	if !w.appEnabled || w.operatorLogin == "" {
		return base
	}
	return operatorAttributionResolver{
		operatorLogin: w.operatorLogin,
		fallback:      base,
	}
}

type operatorAttributionResolver struct {
	operatorLogin string
	fallback      ports.ScopedIdentityResolver
}

func (r operatorAttributionResolver) AuthenticatedIdentityForProvider(ctx context.Context, provider, host string) (ports.SCMIdentity, error) {
	if strings.EqualFold(strings.TrimSpace(provider), "github") {
		return ports.SCMIdentity{Login: r.operatorLogin, Human: true}, nil
	}
	if r.fallback == nil {
		return ports.SCMIdentity{}, nil
	}
	return r.fallback.AuthenticatedIdentityForProvider(ctx, provider, host)
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
