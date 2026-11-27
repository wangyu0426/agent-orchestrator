// Package githubbroker implements a repo-scoped GitHub App credential broker
// for AO worker sessions.
//
// Each worker session receives an unguessable per-session capability token
// bound to its session ID and registered repository identities. Workers call
// the broker on demand (via the Git credential helper and gh wrapper) to obtain
// short-lived, least-privilege, single-repository GitHub App installation
// tokens without ever seeing the App private key or broad daemon tokens.
package githubbroker

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubapp"
)

const (
	// DefaultGitHubHost is the default GitHub host served by the broker.
	DefaultGitHubHost = "github.com"
	// GitUsername is the HTTP Basic username required by GitHub for App installation tokens.
	GitUsername = "x-access-token"
)

var (
	// ErrDisabled indicates the worker credential broker is not enabled.
	ErrDisabled = errors.New("githubbroker: credential broker is disabled")
	// ErrUnauthorized indicates missing, invalid, or revoked session broker credentials.
	ErrUnauthorized = errors.New("githubbroker: unauthorized session credential")
	// ErrForbiddenRepo indicates the requested repository is not bound to the session or allowed by the App.
	ErrForbiddenRepo = errors.New("githubbroker: repository not authorized for session")
	// ErrInvalidRequest indicates a malformed broker request.
	ErrInvalidRequest = errors.New("githubbroker: invalid request")
	// ErrUnsupportedIsolation indicates the host/sandbox configuration does not isolate the private key from workers.
	ErrUnsupportedIsolation = errors.New("githubbroker: unsupported worker isolation configuration")
)

// DefaultWorkerPermissions defines the least-privilege GitHub App permissions
// minted for worker git push and gh PR/API operations. Administrative,
// workflow, and organization permissions are intentionally excluded.
var DefaultWorkerPermissions = map[string]string{
	"contents":      "write",
	"pull_requests": "write",
	"issues":        "write",
	"metadata":      "read",
	"checks":        "read",
	"statuses":      "read",
}

// TokenMinter is the subset of githubapp.Provider used by the broker.
type TokenMinter interface {
	MintScopedToken(ctx context.Context, req githubapp.ScopedTokenRequest) (githubapp.ScopedToken, error)
	InvalidateScopedToken(req githubapp.ScopedTokenRequest)
}

// Config configures a worker credential Broker.
type Config struct {
	Provider       TokenMinter
	AllowedRepos   []string
	GitHubHost     string
	Permissions    map[string]string
	PrivateKeyPath string
	IsolationMode  string
	// GrantsPath optionally persists session grant verifiers (never raw tokens)
	// so surviving live workers stay authorized across daemon restarts.
	GrantsPath string
	Logger     *slog.Logger
}

type sessionGrant struct {
	sessionID domain.SessionID
	projectID domain.ProjectID
	verifier  string
	// allowedRepos maps normalized lowercase "owner/repo" -> repo name (without owner).
	allowedRepos map[string]string
	// canonicalFull maps normalized lowercase "owner/repo" -> original "owner/repo".
	canonicalFull map[string]string
	revoked       bool
}

// Broker authenticates worker sessions and mints least-privilege,
// single-repository GitHub App installation tokens on demand.
type Broker struct {
	provider TokenMinter
	// allowedRepos maps normalized lowercase "owner/repo" -> repo name (without owner).
	allowedRepos  map[string]string
	githubHost    string
	permissions   map[string]string
	grantsPath    string
	logger        *slog.Logger
	sessionActive func(ctx context.Context, id domain.SessionID) (bool, error)

	mu     sync.RWMutex
	grants map[domain.SessionID]*sessionGrant
}

// IssueRequest is a broker token request from an authenticated worker session.
type IssueRequest struct {
	SessionID   domain.SessionID
	BrokerToken string
	Host        string
	// Repository is "owner/repo" (optional only when the session has exactly one registered repo).
	Repository string
}

// IssueResponse is a short-lived, repo-scoped GitHub App credential for a worker.
type IssueResponse struct {
	Token      string    `json:"token"`
	ExpiresAt  time.Time `json:"expires_at"`
	Username   string    `json:"username"`
	Host       string    `json:"host"`
	Repository string    `json:"repository"`
}

// String implements fmt.Stringer without exposing the token value.
func (r IssueResponse) String() string {
	return fmt.Sprintf("githubbroker.IssueResponse(host=%q, repo=%q, expires_at=%s, token=[REDACTED])",
		r.Host, r.Repository, r.ExpiresAt.UTC().Format(time.RFC3339))
}

// GoString implements fmt.GoStringer without exposing the token value.
func (r IssueResponse) GoString() string {
	return r.String()
}

// New validates cfg, enforces the worker isolation check, and returns a Broker.
func New(cfg Config) (*Broker, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("%w: token provider is required", ErrInvalidRequest)
	}
	if len(cfg.AllowedRepos) == 0 {
		return nil, fmt.Errorf("%w: at least one owner/repo entry is required in AllowedRepos", ErrInvalidRequest)
	}
	allowed := make(map[string]string, len(cfg.AllowedRepos))
	for _, raw := range cfg.AllowedRepos {
		full, repoName, err := parseOwnerRepo(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
		allowed[strings.ToLower(full)] = repoName
	}
	if err := ValidateIsolation(cfg.IsolationMode, cfg.PrivateKeyPath); err != nil {
		return nil, err
	}

	host := strings.ToLower(strings.TrimSpace(cfg.GitHubHost))
	if host == "" {
		host = DefaultGitHubHost
	}
	perms := cloneMap(cfg.Permissions)
	if len(perms) == 0 {
		perms = cloneMap(DefaultWorkerPermissions)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if strings.EqualFold(strings.TrimSpace(cfg.IsolationMode), "unsafe-same-uid") {
		logger.Warn("github app worker broker running with unsafe-same-uid override; same-UID workers share filesystem access with daemon")
	}

	b := &Broker{
		provider:     cfg.Provider,
		allowedRepos: allowed,
		githubHost:   host,
		permissions:  perms,
		grantsPath:   strings.TrimSpace(cfg.GrantsPath),
		logger:       logger,
		grants:       make(map[domain.SessionID]*sessionGrant),
	}
	if err := b.loadGrants(); err != nil {
		return nil, fmt.Errorf("githubbroker: load persisted grants: %w", err)
	}
	return b, nil
}

// GitHubHost returns the GitHub host served by this broker.
func (b *Broker) GitHubHost() string {
	if b == nil {
		return DefaultGitHubHost
	}
	return b.githubHost
}

// SetSessionActiveChecker installs a durable session-liveness check used both
// to reconcile persisted grants on startup and to fail closed on IssueToken
// when a session has been terminated out-of-band (for example by tracker facts).
func (b *Broker) SetSessionActiveChecker(fn func(ctx context.Context, id domain.SessionID) (bool, error)) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.sessionActive = fn
	b.mu.Unlock()
}

// ReconcileSessions prunes any persisted grants whose sessions are missing or
// terminated according to the configured session-active checker.
func (b *Broker) ReconcileSessions(ctx context.Context) {
	if b == nil {
		return
	}
	b.mu.RLock()
	checker := b.sessionActive
	if checker == nil {
		b.mu.RUnlock()
		return
	}
	ids := make([]domain.SessionID, 0, len(b.grants))
	for id := range b.grants {
		ids = append(ids, id)
	}
	b.mu.RUnlock()

	for _, id := range ids {
		active, err := checker(ctx, id)
		if err != nil {
			b.logger.Warn("githubbroker: skipping reconcile for session due to lookup error", "session", id, "error", err)
			continue
		}
		if !active {
			b.RevokeSession(id)
		}
	}
}

// RegisterSession binds a fresh unguessable broker capability token to
// sessionID and its registered repository identities ("owner/repo").
func (b *Broker) RegisterSession(sessionID domain.SessionID, projectID domain.ProjectID, registeredRepos []string) (string, error) {
	if b == nil {
		return "", ErrDisabled
	}
	if strings.TrimSpace(string(sessionID)) == "" {
		return "", fmt.Errorf("%w: session_id is required", ErrInvalidRequest)
	}
	if len(registeredRepos) == 0 {
		return "", fmt.Errorf("%w: at least one registered repository is required", ErrForbiddenRepo)
	}

	allowedMap := make(map[string]string, len(registeredRepos))
	canonicalMap := make(map[string]string, len(registeredRepos))
	for _, raw := range registeredRepos {
		full, repoName, err := parseOwnerRepo(raw)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrForbiddenRepo, err)
		}
		key := strings.ToLower(full)
		if _, ok := b.allowedRepos[key]; !ok {
			return "", fmt.Errorf("%w: repository %q is not in the GitHub App allowlist", ErrForbiddenRepo, full)
		}
		allowedMap[key] = repoName
		canonicalMap[key] = full
	}

	raw := make([]byte, sha256.Size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("githubbroker: generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	b.mu.Lock()
	prev, hadPrev := b.grants[sessionID]
	b.grants[sessionID] = &sessionGrant{
		sessionID:     sessionID,
		projectID:     projectID,
		verifier:      tokenVerifier(sessionID, token),
		allowedRepos:  allowedMap,
		canonicalFull: canonicalMap,
	}
	if persistErr := b.persistGrantsLocked(); persistErr != nil {
		if hadPrev {
			b.grants[sessionID] = prev
		} else {
			delete(b.grants, sessionID)
		}
		b.mu.Unlock()
		return "", fmt.Errorf("githubbroker: persist session grant: %w", persistErr)
	}
	b.mu.Unlock()

	return token, nil
}

// RevokeSession immediately revokes the broker capability for sessionID.
func (b *Broker) RevokeSession(sessionID domain.SessionID) {
	if b == nil || sessionID == "" {
		return
	}
	b.mu.Lock()
	delete(b.grants, sessionID)
	persistErr := b.persistGrantsLocked()
	if persistErr != nil && b.grantsPath != "" {
		// Fail closed: remove the stale grants file so a daemon restart cannot
		// reload a revoked grant.
		_ = os.Remove(b.grantsPath)
	}
	b.mu.Unlock()
	if persistErr != nil {
		b.logger.Warn("githubbroker: failed to persist session revocation; removed grants file to fail closed", "session", sessionID, "error", persistErr)
	}
}

// IssueToken authenticates the session caller, validates the requested
// repository against the session's registered repositories and App allowlist,
// and mints a least-privilege single-repository installation token.
func (b *Broker) IssueToken(ctx context.Context, req IssueRequest) (IssueResponse, error) {
	if b == nil {
		return IssueResponse{}, ErrDisabled
	}
	fullRepo, repoName, err := b.authorize(req)
	if err != nil {
		return IssueResponse{}, err
	}
	b.mu.RLock()
	checker := b.sessionActive
	b.mu.RUnlock()
	if checker != nil {
		active, checkErr := checker(ctx, req.SessionID)
		if checkErr != nil {
			return IssueResponse{}, fmt.Errorf("githubbroker: verify session state: %w", checkErr)
		}
		if !active {
			b.RevokeSession(req.SessionID)
			return IssueResponse{}, ErrUnauthorized
		}
	}

	mintReq := githubapp.ScopedTokenRequest{
		Repositories: []string{repoName},
		Permissions:  cloneMap(b.permissions),
	}
	scoped, err := b.provider.MintScopedToken(ctx, mintReq)
	if err != nil {
		return IssueResponse{}, fmt.Errorf("githubbroker: mint token: %w", errors.New(githubapp.Redact(err.Error())))
	}
	// Verify that the GitHub installation actually minted a token bound to the
	// exact authorized owner/repo. This fails closed if AO_GITHUB_APP_INSTALLATION_OWNER
	// is misconfigured for the installation ID or if GitHub returns a different repo.
	if len(scoped.FullNames) != 1 || !strings.EqualFold(scoped.FullNames[0], fullRepo) {
		b.provider.InvalidateScopedToken(mintReq)
		return IssueResponse{}, fmt.Errorf("%w: minted token scope %v does not match authorized repository %q", ErrForbiddenRepo, scoped.FullNames, fullRepo)
	}

	return IssueResponse{
		Token:      scoped.Token,
		ExpiresAt:  scoped.ExpiresAt,
		Username:   GitUsername,
		Host:       b.githubHost,
		Repository: fullRepo,
	}, nil
}

// InvalidateToken authenticates the caller and evicts the cached token for the
// session's authorized repository scope so the next request mints a fresh one.
func (b *Broker) InvalidateToken(_ context.Context, req IssueRequest) error {
	if b == nil {
		return ErrDisabled
	}
	_, repoName, err := b.authorize(req)
	if err != nil {
		return err
	}
	b.provider.InvalidateScopedToken(githubapp.ScopedTokenRequest{
		Repositories: []string{repoName},
		Permissions:  cloneMap(b.permissions),
	})
	return nil
}

func (b *Broker) authorize(req IssueRequest) (fullRepo, repoName string, err error) {
	if strings.TrimSpace(string(req.SessionID)) == "" || strings.TrimSpace(req.BrokerToken) == "" {
		return "", "", ErrUnauthorized
	}

	host := strings.ToLower(strings.TrimSpace(req.Host))
	if host != "" && host != b.githubHost {
		return "", "", fmt.Errorf("%w: unsupported host %q", ErrInvalidRequest, host)
	}

	b.mu.RLock()
	grant, ok := b.grants[req.SessionID]
	if !ok || grant.revoked || grant.verifier == "" {
		b.mu.RUnlock()
		return "", "", ErrUnauthorized
	}
	expected := grant.verifier
	allowedRepos := grant.allowedRepos
	canonicalFull := grant.canonicalFull
	b.mu.RUnlock()

	presented := tokenVerifier(req.SessionID, req.BrokerToken)
	if !hmac.Equal([]byte(expected), []byte(presented)) {
		return "", "", ErrUnauthorized
	}

	rawRepo := strings.TrimSpace(req.Repository)
	if rawRepo == "" {
		if len(allowedRepos) == 1 {
			for k, name := range allowedRepos {
				return canonicalFull[k], name, nil
			}
		}
		return "", "", fmt.Errorf("%w: repository is required for multi-repository session", ErrInvalidRequest)
	}

	parsedFull, _, err := parseOwnerRepo(rawRepo)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrForbiddenRepo, err)
	}
	key := strings.ToLower(parsedFull)
	boundName, ok := allowedRepos[key]
	if !ok {
		return "", "", fmt.Errorf("%w: repository %q is not bound to session %s", ErrForbiddenRepo, parsedFull, req.SessionID)
	}
	if _, ok := b.allowedRepos[key]; !ok {
		return "", "", fmt.Errorf("%w: repository %q is not in the GitHub App allowlist", ErrForbiddenRepo, parsedFull)
	}
	return canonicalFull[key], boundName, nil
}

func tokenVerifier(sessionID domain.SessionID, token string) string {
	h := sha256.New()
	_, _ = h.Write([]byte("ao-github-broker-v1\x00"))
	_, _ = h.Write([]byte(sessionID))
	_, _ = h.Write([]byte{'\x00'})
	_, _ = h.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// parseOwnerRepo normalizes "owner/repo", "/owner/repo.git", etc. into ("owner/repo", "repo").
func parseOwnerRepo(raw string) (string, string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "/")
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repository %q must be in owner/repo format", raw)
	}
	if parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return "", "", fmt.Errorf("repository %q contains invalid path segments", raw)
	}
	return parts[0] + "/" + parts[1], parts[1], nil
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type persistedGrant struct {
	SessionID domain.SessionID `json:"session_id"`
	ProjectID domain.ProjectID `json:"project_id"`
	Verifier  string           `json:"verifier"`
	Repos     []string         `json:"repos"`
}

func (b *Broker) loadGrants() error {
	if b.grantsPath == "" {
		return nil
	}
	data, err := os.ReadFile(b.grantsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var records []persistedGrant
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}
	for _, rec := range records {
		if rec.SessionID == "" || rec.Verifier == "" || len(rec.Repos) == 0 {
			continue
		}
		allowedMap := make(map[string]string, len(rec.Repos))
		canonicalMap := make(map[string]string, len(rec.Repos))
		for _, raw := range rec.Repos {
			full, repoName, err := parseOwnerRepo(raw)
			if err != nil {
				continue
			}
			key := strings.ToLower(full)
			if _, ok := b.allowedRepos[key]; !ok {
				continue
			}
			allowedMap[key] = repoName
			canonicalMap[key] = full
		}
		if len(allowedMap) == 0 {
			continue
		}
		b.grants[rec.SessionID] = &sessionGrant{
			sessionID:     rec.SessionID,
			projectID:     rec.ProjectID,
			verifier:      rec.Verifier,
			allowedRepos:  allowedMap,
			canonicalFull: canonicalMap,
		}
	}
	return nil
}

func (b *Broker) persistGrantsLocked() error {
	if b.grantsPath == "" {
		return nil
	}
	records := make([]persistedGrant, 0, len(b.grants))
	for _, g := range b.grants {
		if g.revoked || g.verifier == "" {
			continue
		}
		repos := make([]string, 0, len(g.canonicalFull))
		for _, full := range g.canonicalFull {
			repos = append(repos, full)
		}
		records = append(records, persistedGrant{
			SessionID: g.sessionID,
			ProjectID: g.projectID,
			Verifier:  g.verifier,
			Repos:     repos,
		})
	}
	data, err := json.Marshal(records)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.grantsPath), 0o700); err != nil {
		return err
	}
	tmpPath := b.grantsPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, b.grantsPath)
}
