package githubbroker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubapp"
)

func generateTestKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func newMockGitHubProvider(t *testing.T, clock func() time.Time, handler func(w http.ResponseWriter, r *http.Request)) (*githubapp.Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)

	p, err := githubapp.NewProvider(githubapp.Config{
		AppID:          42,
		InstallationID: 99,
		PrivateKeyPEM:  generateTestKeyPEM(t),
		BaseURL:        srv.URL,
		Clock:          clock,
		RefreshSkew:    2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p, srv
}

func TestBroker_ScopedIssuanceExpiryRefreshAndConcurrency(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}

	var calls atomic.Int32
	var lastRepos []string
	var lastPerms map[string]string

	provider, _ := newMockGitHubProvider(t, clock, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		lastRepos = body.Repositories
		lastPerms = body.Permissions
		exp := now.Add(1 * time.Hour)
		mu.Unlock()

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":        fmt.Sprintf("ghs_scoped_token_%02d_abcdefghij", n),
			"expires_at":   exp.Format(time.RFC3339),
			"repositories": []map[string]string{{"full_name": "wangyu0426/agent-orchestrator"}},
		})
	})

	broker, err := New(Config{
		Provider:     provider,
		AllowedRepos: []string{"wangyu0426/agent-orchestrator", "wangyu0426/other-repo"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sessToken, err := broker.RegisterSession("sess-1", "proj-1", []string{"wangyu0426/agent-orchestrator"})
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}

	// Concurrent requests should deduplicate to a single GitHub token exchange.
	const workers = 12
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	tokens := make(chan string, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := broker.IssueToken(context.Background(), IssueRequest{
				SessionID:   "sess-1",
				BrokerToken: sessToken,
				Host:        "github.com",
				Repository:  "wangyu0426/agent-orchestrator",
			})
			if err != nil {
				errs <- err
				return
			}
			tokens <- resp.Token
		}()
	}
	wg.Wait()
	close(errs)
	close(tokens)

	for err := range errs {
		t.Fatalf("concurrent IssueToken failed: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 GitHub token exchange for concurrent requests, got %d", got)
	}
	for tok := range tokens {
		if tok != "ghs_scoped_token_01_abcdefghij" {
			t.Fatalf("unexpected token %q", tok)
		}
	}
	if len(lastRepos) != 1 || lastRepos[0] != "agent-orchestrator" {
		t.Fatalf("expected single-repo scope [agent-orchestrator], got %v", lastRepos)
	}
	if lastPerms["contents"] != "write" || lastPerms["pull_requests"] != "write" || lastPerms["administration"] != "" {
		t.Fatalf("unexpected least-privilege permissions: %v", lastPerms)
	}

	// Advance clock past refresh skew (59 minutes later -> within 2m skew of 1h expiry).
	mu.Lock()
	now = now.Add(59 * time.Minute)
	mu.Unlock()

	refreshed, err := broker.IssueToken(context.Background(), IssueRequest{
		SessionID:   "sess-1",
		BrokerToken: sessToken,
		Repository:  "wangyu0426/agent-orchestrator",
	})
	if err != nil {
		t.Fatalf("IssueToken after expiry: %v", err)
	}
	if refreshed.Token != "ghs_scoped_token_02_abcdefghij" {
		t.Fatalf("expected refreshed token 02, got %q", refreshed.Token)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 GitHub token exchanges after refresh, got %d", got)
	}
}

func TestBroker_UnauthorizedRepoSessionAndRevocation(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	provider, _ := newMockGitHubProvider(t, func() time.Time { return now }, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":        "ghs_valid_token_000000000000",
			"expires_at":   now.Add(1 * time.Hour).Format(time.RFC3339),
			"repositories": []map[string]string{{"full_name": "org/repo-a"}},
		})
	})

	broker, err := New(Config{
		Provider:     provider,
		AllowedRepos: []string{"org/repo-a", "org/repo-b"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Cross-owner repo with the same repo name must be rejected at registration.
	if _, err := broker.RegisterSession("sess-wrong-owner", "proj-x", []string{"evil-org/repo-a"}); !errors.Is(err, ErrForbiddenRepo) {
		t.Fatalf("expected ErrForbiddenRepo for different owner with same repo name, got %v", err)
	}

	tokA, err := broker.RegisterSession("sess-a", "proj-a", []string{"org/repo-a"})
	if err != nil {
		t.Fatalf("RegisterSession A: %v", err)
	}
	tokB, err := broker.RegisterSession("sess-b", "proj-b", []string{"org/repo-b"})
	if err != nil {
		t.Fatalf("RegisterSession B: %v", err)
	}

	// Cross-session token misuse must fail.
	if _, err := broker.IssueToken(context.Background(), IssueRequest{
		SessionID:   "sess-a",
		BrokerToken: tokB,
		Repository:  "org/repo-a",
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for cross-session token, got %v", err)
	}

	// Arbitrary/other-session repository request must fail.
	if _, err := broker.IssueToken(context.Background(), IssueRequest{
		SessionID:   "sess-a",
		BrokerToken: tokA,
		Repository:  "org/repo-b",
	}); !errors.Is(err, ErrForbiddenRepo) {
		t.Fatalf("expected ErrForbiddenRepo for other repo, got %v", err)
	}

	// Revoked session must fail immediately.
	broker.RevokeSession("sess-a")
	if _, err := broker.IssueToken(context.Background(), IssueRequest{
		SessionID:   "sess-a",
		BrokerToken: tokA,
		Repository:  "org/repo-a",
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized after RevokeSession, got %v", err)
	}
}

func TestBroker_RedactionAndIsolationFailClosed(t *testing.T) {
	// Response String/GoString must never expose the token.
	resp := IssueResponse{
		Token:      "ghs_super_secret_token_1234567890",
		ExpiresAt:  time.Date(2026, 11, 27, 13, 0, 0, 0, time.UTC),
		Username:   GitUsername,
		Host:       DefaultGitHubHost,
		Repository: "org/repo-a",
	}
	if strings.Contains(resp.String(), "ghs_super_secret") || strings.Contains(resp.GoString(), "ghs_super_secret") {
		t.Fatalf("IssueResponse leaked token in String/GoString: %s", resp.String())
	}

	// Strict isolation must fail closed when private key file is readable by the same UID.
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, generateTestKeyPEM(t), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if err := ValidateIsolation("strict", keyPath); !errors.Is(err, ErrUnsupportedIsolation) {
		t.Fatalf("expected ErrUnsupportedIsolation in strict mode for same-UID readable key, got %v", err)
	}
	if err := ValidateIsolation("external-sandbox", keyPath); err != nil {
		t.Fatalf("expected external-sandbox to pass, got %v", err)
	}
	if err := ValidateIsolation("unsafe-same-uid", keyPath); err != nil {
		t.Fatalf("expected unsafe-same-uid to pass, got %v", err)
	}
}

func TestBroker_FailsClosedOnInstallationOwnerMismatch(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	provider, _ := newMockGitHubProvider(t, func() time.Time { return now }, func(w http.ResponseWriter, r *http.Request) {
		// Simulate misconfigured AO_GITHUB_APP_INSTALLATION_OWNER: installation
		// actually owns "other-owner/agent-orchestrator", not "wangyu0426/agent-orchestrator".
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":        "ghs_wrong_owner_token_1234567890",
			"expires_at":   now.Add(1 * time.Hour).Format(time.RFC3339),
			"repositories": []map[string]string{{"full_name": "other-owner/agent-orchestrator"}},
		})
	})

	broker, err := New(Config{
		Provider:     provider,
		AllowedRepos: []string{"wangyu0426/agent-orchestrator"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sessToken, err := broker.RegisterSession("sess-mismatch", "proj-1", []string{"wangyu0426/agent-orchestrator"})
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	if _, err := broker.IssueToken(context.Background(), IssueRequest{
		SessionID:   "sess-mismatch",
		BrokerToken: sessToken,
		Repository:  "wangyu0426/agent-orchestrator",
	}); !errors.Is(err, ErrForbiddenRepo) {
		t.Fatalf("expected ErrForbiddenRepo when installation owner mismatches, got %v", err)
	}
}

func TestBroker_PersistsGrantVerifiersAcrossDaemonRestart(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	provider, _ := newMockGitHubProvider(t, func() time.Time { return now }, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":        "ghs_restarted_token_1234567890",
			"expires_at":   now.Add(1 * time.Hour).Format(time.RFC3339),
			"repositories": []map[string]string{{"full_name": "wangyu0426/agent-orchestrator"}},
		})
	})

	grantsPath := filepath.Join(t.TempDir(), "state", "github-broker-grants.json")
	broker1, err := New(Config{
		Provider:     provider,
		AllowedRepos: []string{"wangyu0426/agent-orchestrator"},
		GrantsPath:   grantsPath,
	})
	if err != nil {
		t.Fatalf("New broker1: %v", err)
	}
	sessToken, err := broker1.RegisterSession("sess-surviving", "proj-1", []string{"wangyu0426/agent-orchestrator"})
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}

	// Verify raw broker token is never persisted on disk.
	rawDisk, err := os.ReadFile(grantsPath)
	if err != nil {
		t.Fatalf("read grants file: %v", err)
	}
	if strings.Contains(string(rawDisk), sessToken) {
		t.Fatalf("raw broker token leaked to grants file")
	}

	// Simulate daemon restart by constructing a fresh Broker with the same GrantsPath.
	broker2, err := New(Config{
		Provider:     provider,
		AllowedRepos: []string{"wangyu0426/agent-orchestrator"},
		GrantsPath:   grantsPath,
	})
	if err != nil {
		t.Fatalf("New broker2: %v", err)
	}
	resp, err := broker2.IssueToken(context.Background(), IssueRequest{
		SessionID:   "sess-surviving",
		BrokerToken: sessToken,
		Repository:  "wangyu0426/agent-orchestrator",
	})
	if err != nil || resp.Token != "ghs_restarted_token_1234567890" {
		t.Fatalf("expected surviving session to remain authorized after restart, got resp=%+v err=%v", resp, err)
	}

	// Revocation persists across restart too.
	broker2.RevokeSession("sess-surviving")
	broker3, err := New(Config{
		Provider:     provider,
		AllowedRepos: []string{"wangyu0426/agent-orchestrator"},
		GrantsPath:   grantsPath,
	})
	if err != nil {
		t.Fatalf("New broker3: %v", err)
	}
	if _, err := broker3.IssueToken(context.Background(), IssueRequest{
		SessionID:   "sess-surviving",
		BrokerToken: sessToken,
		Repository:  "wangyu0426/agent-orchestrator",
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected revoked session to stay unauthorized after restart, got %v", err)
	}

	// Transient lookup errors fail closed without revoking the live grant.
	tokTransient, err := broker3.RegisterSession("sess-transient", "proj-1", []string{"wangyu0426/agent-orchestrator"})
	if err != nil {
		t.Fatalf("RegisterSession transient: %v", err)
	}
	broker3.SetSessionActiveChecker(func(_ context.Context, _ domain.SessionID) (bool, error) {
		return false, context.Canceled
	})
	if _, err := broker3.IssueToken(context.Background(), IssueRequest{
		SessionID:   "sess-transient",
		BrokerToken: tokTransient,
		Repository:  "wangyu0426/agent-orchestrator",
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on transient lookup error, got %v", err)
	}
	broker3.SetSessionActiveChecker(func(_ context.Context, _ domain.SessionID) (bool, error) {
		return true, nil
	})
	if resp, err := broker3.IssueToken(context.Background(), IssueRequest{
		SessionID:   "sess-transient",
		BrokerToken: tokTransient,
		Repository:  "wangyu0426/agent-orchestrator",
	}); err != nil || resp.Token != "ghs_restarted_token_1234567890" {
		t.Fatalf("expected live grant to survive transient lookup error, got resp=%+v err=%v", resp, err)
	}

	// Out-of-band termination (e.g. tracker-closed session) is pruned on reconcile
	// and rejected fail-closed on IssueToken.
	tok2, err := broker3.RegisterSession("sess-tracker-closed", "proj-1", []string{"wangyu0426/agent-orchestrator"})
	if err != nil {
		t.Fatalf("RegisterSession tracker-closed: %v", err)
	}
	broker3.SetSessionActiveChecker(func(_ context.Context, id domain.SessionID) (bool, error) {
		return id != "sess-tracker-closed", nil
	})
	if _, err := broker3.IssueToken(context.Background(), IssueRequest{
		SessionID:   "sess-tracker-closed",
		BrokerToken: tok2,
		Repository:  "wangyu0426/agent-orchestrator",
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for out-of-band terminated session, got %v", err)
	}
}
