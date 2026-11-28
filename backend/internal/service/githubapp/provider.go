// Package githubapp provides a shared GitHub App installation-token provider
// for the AO daemon's SCM and Tracker adapters.
//
// It generates short-lived RS256 JWTs signed with the configured RSA private
// key, exchanges them for scoped installation access tokens via GitHub's REST
// API, caches tokens per repository scope, refreshes ahead of expires_at,
// deduplicates concurrent refreshes, and supports invalidation on auth failure.
//
// App mode is fail-closed: errors never fall back to personal tokens or gh CLI.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	defaultBaseURL     = "https://api.github.com"
	defaultHTTPTimeout = 15 * time.Second
	defaultRefreshSkew = 2 * time.Minute
	defaultUserAgent   = "ao-agent-orchestrator/github-app"
	jwtIssuedAtSkew    = 60 * time.Second
	jwtLifetime        = 9 * time.Minute // GitHub maximum is 10 minutes
	maxResponseBytes   = 1 << 20         // 1 MiB cap on token-exchange responses
)

var (
	// ErrInvalidConfig indicates missing or malformed GitHub App configuration.
	ErrInvalidConfig = errors.New("githubapp: invalid configuration")
	// ErrAuthFailed indicates GitHub rejected the App JWT or installation credentials.
	ErrAuthFailed = errors.New("githubapp: authentication failed")
	// ErrTokenExchange indicates a non-auth failure exchanging the JWT for an installation token.
	ErrTokenExchange = errors.New("githubapp: installation token exchange failed")
)

// Config configures a GitHub App installation token Provider.
type Config struct {
	AppID          int64
	InstallationID int64
	// PrivateKeyPath is the filesystem path to the PEM-encoded RSA private key.
	PrivateKeyPath string
	// PrivateKeyPEM allows injecting PEM bytes directly (primarily for tests).
	// When non-empty, PrivateKeyPath is not read.
	PrivateKeyPEM []byte
	// BaseURL overrides the GitHub REST API base URL (default: https://api.github.com).
	BaseURL string
	// HTTPClient overrides the HTTP client (default: 15s timeout).
	HTTPClient *http.Client
	// Clock overrides time.Now for deterministic testing.
	Clock func() time.Time
	// RefreshSkew is how far ahead of expires_at a cached token is refreshed.
	// Zero defaults to 2 minutes.
	RefreshSkew time.Duration
	// UserAgent overrides the User-Agent header.
	UserAgent string
	// Logger is optional; defaults to slog.Default().
	Logger *slog.Logger
}

type cachedToken struct {
	token     string
	expiresAt time.Time
	fullNames []string
}

type inflightRefresh struct {
	done      chan struct{}
	token     string
	expiresAt time.Time
	fullNames []string
	err       error
}

// ScopedTokenRequest describes a repo-scoped, optionally permission-restricted
// installation access token request.
type ScopedTokenRequest struct {
	Repositories []string
	Permissions  map[string]string
}

// ScopedToken is a minted installation access token with its expiry metadata.
type ScopedToken struct {
	Token        string
	ExpiresAt    time.Time
	Repositories []string
	FullNames    []string
	Permissions  map[string]string
}

// String implements fmt.Stringer without exposing the token value.
func (t ScopedToken) String() string {
	return fmt.Sprintf("githubapp.ScopedToken(repos=%v, expires_at=%s, token=[REDACTED])", t.Repositories, t.ExpiresAt.UTC().Format(time.RFC3339))
}

// GoString implements fmt.GoStringer without exposing the token value.
func (t ScopedToken) GoString() string {
	return t.String()
}

// Provider manages RS256 JWT signing and scoped installation access token
// caching for a single GitHub App installation.
type Provider struct {
	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey
	baseURL        string
	httpClient     *http.Client
	clock          func() time.Time
	refreshSkew    time.Duration
	userAgent      string
	logger         *slog.Logger
	actorMu        sync.Mutex
	actorLogin     string

	mu       sync.Mutex
	cache    map[string]cachedToken
	inflight map[string]*inflightRefresh
}

// NewProvider validates cfg, loads the RSA private key, and returns a Provider.
// It fails closed on any missing or malformed credential.
func NewProvider(cfg Config) (*Provider, error) {
	if cfg.AppID <= 0 {
		return nil, fmt.Errorf("%w: app_id must be a positive integer", ErrInvalidConfig)
	}
	if cfg.InstallationID <= 0 {
		return nil, fmt.Errorf("%w: installation_id must be a positive integer", ErrInvalidConfig)
	}

	pemBytes := cfg.PrivateKeyPEM
	if len(pemBytes) == 0 {
		path := strings.TrimSpace(cfg.PrivateKeyPath)
		if path == "" {
			return nil, fmt.Errorf("%w: private_key_path is required", ErrInvalidConfig)
		}
		var err error
		pemBytes, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%w: read private key file: %v", ErrInvalidConfig, Redact(err.Error()))
		}
	}

	key, err := parseRSAPrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if err := validateBaseURL(baseURL); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	refreshSkew := cfg.RefreshSkew
	if refreshSkew <= 0 {
		refreshSkew = defaultRefreshSkew
	}
	userAgent := strings.TrimSpace(cfg.UserAgent)
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Provider{
		appID:          cfg.AppID,
		installationID: cfg.InstallationID,
		privateKey:     key,
		baseURL:        baseURL,
		httpClient:     httpClient,
		clock:          clock,
		refreshSkew:    refreshSkew,
		userAgent:      userAgent,
		logger:         logger,
		cache:          make(map[string]cachedToken),
		inflight:       make(map[string]*inflightRefresh),
	}, nil
}

// String implements fmt.Stringer without exposing any key material or tokens.
func (p *Provider) String() string {
	if p == nil {
		return "githubapp.Provider(<nil>)"
	}
	return fmt.Sprintf("githubapp.Provider(app_id=%d, installation_id=%d)", p.appID, p.installationID)
}

// GoString implements fmt.GoStringer without exposing any key material or tokens.
func (p *Provider) GoString() string {
	return p.String()
}

// SignJWT creates a short-lived RS256 JWT for authenticating as the GitHub App.
func (p *Provider) SignJWT(now time.Time) (string, error) {
	headerJSON := `{"alg":"RS256","typ":"JWT"}`
	claims := struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Issuer    string `json:"iss"`
	}{
		IssuedAt:  now.Add(-jwtIssuedAtSkew).Unix(),
		ExpiresAt: now.Add(jwtLifetime).Unix(),
		Issuer:    strconv.FormatInt(p.appID, 10),
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("githubapp: encode jwt claims: %w", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString([]byte(headerJSON)) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("githubapp: sign jwt: %w", err)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// AppActorLogin resolves the effective GitHub account used by installation
// tokens for this App. GitHub installation tokens act as the App's bot user;
// comparing App IDs or installation IDs alone would miss two installations of
// the same App, so callers use this login when validating role separation.
func (p *Provider) AppActorLogin(ctx context.Context) (string, error) {
	if p == nil {
		return "", fmt.Errorf("%w: provider is required", ErrInvalidConfig)
	}
	p.actorMu.Lock()
	defer p.actorMu.Unlock()
	if p.actorLogin != "" {
		return p.actorLogin, nil
	}
	jwt, err := p.SignJWT(p.clock())
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/app", http.NoBody)
	if err != nil {
		return "", fmt.Errorf("githubapp: build App identity request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", p.userAgent)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("githubapp: resolve App identity: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("githubapp: read App identity response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("githubapp: resolve App identity: status %d: %s", resp.StatusCode, Redact(extractGitHubMessage(body)))
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(body, &app); err != nil {
		return "", fmt.Errorf("githubapp: decode App identity: %w", err)
	}
	slug := strings.TrimSpace(app.Slug)
	if slug == "" || strings.ContainsAny(slug, " /\\\t\r\n") {
		return "", fmt.Errorf("githubapp: GitHub returned an invalid App slug")
	}
	p.actorLogin = strings.ToLower(slug) + "[bot]"
	return p.actorLogin, nil
}

// TokenForRepositories returns a cached or freshly exchanged installation access
// token scoped to repos. Empty scopes are rejected so App mode fails closed.
func (p *Provider) TokenForRepositories(ctx context.Context, repos []string) (string, error) {
	scoped, err := p.MintScopedToken(ctx, ScopedTokenRequest{Repositories: repos})
	if err != nil {
		return "", err
	}
	return scoped.Token, nil
}

// MintScopedToken returns a cached or freshly exchanged installation access
// token scoped to req.Repositories and optional req.Permissions, along with its
// expiry timestamp.
func (p *Provider) MintScopedToken(ctx context.Context, req ScopedTokenRequest) (ScopedToken, error) {
	normalized, err := normalizeRepositories(req.Repositories)
	if err != nil {
		return ScopedToken{}, err
	}
	perms, err := normalizePermissions(req.Permissions)
	if err != nil {
		return ScopedToken{}, err
	}
	scopeKey := buildScopeKey(normalized, perms)

	now := p.clock()
	p.mu.Lock()
	if entry, ok := p.cache[scopeKey]; ok && p.isFresh(entry, now) {
		tok := ScopedToken{
			Token:        entry.token,
			ExpiresAt:    entry.expiresAt,
			Repositories: append([]string(nil), normalized...),
			FullNames:    append([]string(nil), entry.fullNames...),
			Permissions:  clonePermissions(perms),
		}
		p.mu.Unlock()
		return tok, nil
	}
	if call, ok := p.inflight[scopeKey]; ok {
		p.mu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return ScopedToken{}, call.err
			}
			return ScopedToken{
				Token:        call.token,
				ExpiresAt:    call.expiresAt,
				Repositories: append([]string(nil), normalized...),
				FullNames:    append([]string(nil), call.fullNames...),
				Permissions:  clonePermissions(perms),
			}, nil
		case <-ctx.Done():
			return ScopedToken{}, ctx.Err()
		}
	}

	call := &inflightRefresh{done: make(chan struct{})}
	p.inflight[scopeKey] = call
	p.mu.Unlock()

	// Run the shared token exchange with a provider-owned bounded context so
	// cancellation of the leading caller cannot fail unrelated concurrent
	// waiters whose contexts are still live.
	go func() {
		timeout := defaultHTTPTimeout
		if p.httpClient != nil && p.httpClient.Timeout > 0 {
			timeout = p.httpClient.Timeout
		}
		exchangeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()

		token, expiresAt, fullNames, err := p.exchangeToken(exchangeCtx, normalized, perms)

		p.mu.Lock()
		if err == nil {
			p.cache[scopeKey] = cachedToken{
				token:     token,
				expiresAt: expiresAt,
				fullNames: fullNames,
			}
		}
		delete(p.inflight, scopeKey)
		call.token = token
		call.expiresAt = expiresAt
		call.fullNames = fullNames
		call.err = err
		close(call.done)
		p.mu.Unlock()
	}()

	select {
	case <-call.done:
		if call.err != nil {
			return ScopedToken{}, call.err
		}
		return ScopedToken{
			Token:        call.token,
			ExpiresAt:    call.expiresAt,
			Repositories: append([]string(nil), normalized...),
			FullNames:    append([]string(nil), call.fullNames...),
			Permissions:  clonePermissions(perms),
		}, nil
	case <-ctx.Done():
		return ScopedToken{}, ctx.Err()
	}
}

// InvalidateRepositories drops the cached token for the given repository scope.
func (p *Provider) InvalidateRepositories(repos []string) {
	p.InvalidateScopedToken(ScopedTokenRequest{Repositories: repos})
}

// InvalidateScopedToken drops the cached token for the given repository and
// permission scope.
func (p *Provider) InvalidateScopedToken(req ScopedTokenRequest) {
	normalized, err := normalizeRepositories(req.Repositories)
	if err != nil {
		return
	}
	perms, err := normalizePermissions(req.Permissions)
	if err != nil {
		return
	}
	scopeKey := buildScopeKey(normalized, perms)
	p.mu.Lock()
	delete(p.cache, scopeKey)
	p.mu.Unlock()
}

// InvalidateAll drops all cached installation tokens across all scopes.
func (p *Provider) InvalidateAll() {
	p.mu.Lock()
	clear(p.cache)
	p.mu.Unlock()
}

// TokenSource returns a TokenSource bound to a fixed repository list.
// When repos is empty, the token uses the installation's default repository scope.
func (p *Provider) TokenSource(repos ...string) *ScopedTokenSource {
	fixed := append([]string(nil), repos...)
	return &ScopedTokenSource{
		provider: p,
		repos:    fixed,
	}
}

// DynamicTokenSource returns a TokenSource that resolves the repository scope
// on demand (for example, from the daemon's registered projects).
func (p *Provider) DynamicTokenSource(resolve func(ctx context.Context) ([]string, error)) *ScopedTokenSource {
	return &ScopedTokenSource{
		provider: p,
		resolve:  resolve,
	}
}

// ScopedTokenSource adapts Provider to the SCM and Tracker TokenSource
// interfaces, including cache invalidation, preflight path, and non-human
// identity reporting.
type ScopedTokenSource struct {
	provider *Provider
	repos    []string
	resolve  func(ctx context.Context) ([]string, error)
}

// Token returns a valid installation access token for this source's scope.
func (s *ScopedTokenSource) Token(ctx context.Context) (string, error) {
	if s == nil || s.provider == nil {
		return "", fmt.Errorf("%w: token source not initialized", ErrInvalidConfig)
	}
	repos := s.repos
	if s.resolve != nil {
		var err error
		repos, err = s.resolve(ctx)
		if err != nil {
			return "", fmt.Errorf("githubapp: resolve repository scope: %w", err)
		}
	}
	return s.provider.TokenForRepositories(ctx, repos)
}

// InvalidateToken drops cached installation tokens so the next request
// exchanges a fresh token.
func (s *ScopedTokenSource) InvalidateToken() {
	if s == nil || s.provider == nil {
		return
	}
	if s.resolve != nil {
		s.provider.InvalidateAll()
		return
	}
	s.provider.InvalidateRepositories(s.repos)
}

// PreflightPath returns a GitHub REST endpoint valid for installation tokens.
// Installation tokens cannot call GET /user (GitHub returns 403).
func (s *ScopedTokenSource) PreflightPath() string {
	return "/installation/repositories?per_page=1"
}

// SCMIdentity reports that this token belongs to a GitHub App installation
// rather than a human user account, avoiding GET /user calls.
func (s *ScopedTokenSource) SCMIdentity() (ports.SCMIdentity, bool) {
	return ports.SCMIdentity{Human: false}, true
}

func (p *Provider) isFresh(entry cachedToken, now time.Time) bool {
	if entry.token == "" {
		return false
	}
	return now.Add(p.refreshSkew).Before(entry.expiresAt)
}

func (p *Provider) exchangeToken(ctx context.Context, repos []string, perms map[string]string) (string, time.Time, []string, error) {
	now := p.clock()
	jwt, err := p.SignJWT(now)
	if err != nil {
		return "", time.Time{}, nil, err
	}

	var reqBody io.Reader = http.NoBody
	if len(repos) > 0 || len(perms) > 0 {
		payload, err := json.Marshal(struct {
			Repositories []string          `json:"repositories,omitempty"`
			Permissions  map[string]string `json:"permissions,omitempty"`
		}{
			Repositories: repos,
			Permissions:  perms,
		})
		if err != nil {
			return "", time.Time{}, nil, fmt.Errorf("%w: encode scope payload: %w", ErrTokenExchange, err)
		}
		reqBody = bytes.NewReader(payload)
	}

	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", p.baseURL, p.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reqBody)
	if err != nil {
		return "", time.Time{}, nil, fmt.Errorf("%w: build request: %v", ErrTokenExchange, Redact(err.Error()))
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", p.userAgent)
	if len(repos) > 0 || len(perms) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, nil, fmt.Errorf("%w: %v", ErrTokenExchange, Redact(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", time.Time{}, nil, fmt.Errorf("%w: read response: %v", ErrTokenExchange, Redact(err.Error()))
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		msg := extractGitHubMessage(body)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", time.Time{}, nil, fmt.Errorf("%w: status %d: %s", ErrAuthFailed, resp.StatusCode, Redact(msg))
		}
		return "", time.Time{}, nil, fmt.Errorf("%w: status %d: %s", ErrTokenExchange, resp.StatusCode, Redact(msg))
	}

	var parsed struct {
		Token        string `json:"token"`
		ExpiresAt    string `json:"expires_at"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", time.Time{}, nil, fmt.Errorf("%w: decode response: %v", ErrTokenExchange, Redact(err.Error()))
	}
	token := strings.TrimSpace(parsed.Token)
	if token == "" {
		return "", time.Time{}, nil, fmt.Errorf("%w: empty token in response", ErrTokenExchange)
	}

	if strings.TrimSpace(parsed.ExpiresAt) == "" {
		return "", time.Time{}, nil, fmt.Errorf("%w: missing expires_at in response", ErrTokenExchange)
	}
	expiresAt, err := time.Parse(time.RFC3339, parsed.ExpiresAt)
	if err != nil {
		return "", time.Time{}, nil, fmt.Errorf("%w: invalid expires_at: %w", ErrTokenExchange, err)
	}
	if !expiresAt.After(p.clock()) {
		return "", time.Time{}, nil, fmt.Errorf("%w: token expires_at %s is not in the future", ErrTokenExchange, parsed.ExpiresAt)
	}

	var fullNames []string
	for _, repo := range parsed.Repositories {
		if fn := strings.TrimSpace(repo.FullName); fn != "" {
			fullNames = append(fullNames, fn)
		}
	}

	return token, expiresAt, fullNames, nil
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid base_url: %w", err)
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return errors.New("base_url must include a host")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return errors.New("base_url must use https for non-loopback hosts")
	default:
		return fmt.Errorf("base_url has unsupported scheme %q", u.Scheme)
	}
}

func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("private key is not valid PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("private key must be a valid PKCS#1 or PKCS#8 RSA key")
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key must be an RSA key")
	}
	return rsaKey, nil
}

// ValidateRepositories validates a non-empty list of repository names for
// GitHub's installation access_tokens API.
func ValidateRepositories(repos []string) ([]string, error) {
	return normalizeRepositories(repos)
}

// normalizeRepositories validates and normalizes a non-empty list of repository
// names into a sorted, deduplicated slice for GitHub's installation
// access_tokens API. Empty scopes and owner-qualified or slash-containing
// entries are rejected so App mode fails closed and never mints an
// installation-wide token.
func normalizeRepositories(repos []string) ([]string, error) {
	if len(repos) == 0 {
		return nil, fmt.Errorf("%w: at least one repository scope is required", ErrInvalidConfig)
	}
	seen := make(map[string]struct{}, len(repos))
	out := make([]string, 0, len(repos))
	for _, raw := range repos {
		r := strings.TrimSpace(raw)
		if r == "" {
			return nil, fmt.Errorf("%w: empty repository entry in scope", ErrInvalidConfig)
		}
		if strings.Contains(r, "/") {
			return nil, fmt.Errorf("%w: repository scope %q must be a repository name without owner", ErrInvalidConfig, raw)
		}
		r = strings.TrimSuffix(r, ".git")
		if r == "" {
			return nil, fmt.Errorf("%w: malformed repository scope %q", ErrInvalidConfig, raw)
		}
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	sort.Strings(out)
	return out, nil
}

func normalizePermissions(perms map[string]string) (map[string]string, error) {
	if len(perms) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(perms))
	for k, v := range perms {
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if key == "" || val == "" {
			return nil, fmt.Errorf("%w: permission key and level must be non-empty", ErrInvalidConfig)
		}
		out[key] = val
	}
	return out, nil
}

func buildScopeKey(repos []string, perms map[string]string) string {
	base := strings.Join(repos, ",")
	if len(perms) == 0 {
		return base
	}
	keys := make([]string, 0, len(perms))
	for k := range perms {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+perms[k])
	}
	return base + "|" + strings.Join(parts, ",")
}

func clonePermissions(perms map[string]string) map[string]string {
	if len(perms) == 0 {
		return nil
	}
	out := make(map[string]string, len(perms))
	for k, v := range perms {
		out[k] = v
	}
	return out
}

func extractGitHubMessage(body []byte) string {
	var payload struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &payload) == nil && strings.TrimSpace(payload.Message) != "" {
		return strings.TrimSpace(payload.Message)
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

var (
	ghTokenPattern = regexp.MustCompile(`\b(?:ghs|ghp|gho|ghu|ghr)_[A-Za-z0-9_]{10,}\b|\bgithub_pat_[A-Za-z0-9_]{10,}\b`)
	jwtPattern     = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b`)
	pemPattern     = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
)

// Redact replaces GitHub tokens, JWTs, and PEM private keys with [REDACTED].
func Redact(s string) string {
	s = pemPattern.ReplaceAllString(s, "[REDACTED]")
	s = jwtPattern.ReplaceAllString(s, "[REDACTED]")
	s = ghTokenPattern.ReplaceAllString(s, "[REDACTED]")
	return s
}
