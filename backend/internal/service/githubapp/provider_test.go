package githubapp

import (
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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func generateTestRSAKey(t *testing.T) (*rsa.PrivateKey, []byte, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	pkcs1 := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8Bytes,
	})
	return key, pkcs1, pkcs8
}

func TestNewProvider_Validation(t *testing.T) {
	_, pkcs1, pkcs8 := generateTestRSAKey(t)

	if _, err := NewProvider(Config{InstallationID: 1, PrivateKeyPEM: pkcs1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("missing app_id: got %v, want ErrInvalidConfig", err)
	}
	if _, err := NewProvider(Config{AppID: 1, PrivateKeyPEM: pkcs1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("missing installation_id: got %v, want ErrInvalidConfig", err)
	}
	if _, err := NewProvider(Config{AppID: 1, InstallationID: 2}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("missing key: got %v, want ErrInvalidConfig", err)
	}
	if _, err := NewProvider(Config{AppID: 1, InstallationID: 2, PrivateKeyPEM: []byte("not-a-pem")}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("bad pem: got %v, want ErrInvalidConfig", err)
	}
	if _, err := NewProvider(Config{AppID: 1, InstallationID: 2, PrivateKeyPEM: pkcs1, BaseURL: "http://api.github.com"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("remote http base_url: got %v, want ErrInvalidConfig", err)
	}
	if _, err := NewProvider(Config{AppID: 1, InstallationID: 2, PrivateKeyPEM: pkcs1, BaseURL: "ftp://api.github.com"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("unsupported scheme base_url: got %v, want ErrInvalidConfig", err)
	}
	if _, err := NewProvider(Config{AppID: 1, InstallationID: 2, PrivateKeyPEM: pkcs1, BaseURL: "http://127.0.0.1:8080"}); err != nil {
		t.Fatalf("loopback http base_url rejected: %v", err)
	}
	if _, err := NewProvider(Config{AppID: 1, InstallationID: 2, PrivateKeyPEM: pkcs1, BaseURL: "http://localhost:8080"}); err != nil {
		t.Fatalf("localhost http base_url rejected: %v", err)
	}
	if _, err := NewProvider(Config{AppID: 1, InstallationID: 2, PrivateKeyPEM: pkcs1, BaseURL: "https://github.example.com/api/v3"}); err != nil {
		t.Fatalf("https base_url rejected: %v", err)
	}

	// Both PKCS#1 and PKCS#8 keys must be accepted.
	if _, err := NewProvider(Config{AppID: 123, InstallationID: 456, PrivateKeyPEM: pkcs1}); err != nil {
		t.Fatalf("pkcs1 key rejected: %v", err)
	}
	if _, err := NewProvider(Config{AppID: 123, InstallationID: 456, PrivateKeyPEM: pkcs8}); err != nil {
		t.Fatalf("pkcs8 key rejected: %v", err)
	}

	// Reading from PrivateKeyPath works.
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, pkcs1, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	if _, err := NewProvider(Config{AppID: 123, InstallationID: 456, PrivateKeyPath: keyPath}); err != nil {
		t.Fatalf("key path rejected: %v", err)
	}
}

func TestProvider_SignJWTAndExchange(t *testing.T) {
	key, pkcs1, _ := generateTestRSAKey(t)
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	var seenAuth string
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/99/access_tokens" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		seenAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_test_installation_token_001",
			"expires_at": now.Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	p, err := NewProvider(Config{
		AppID:          42,
		InstallationID: 99,
		PrivateKeyPEM:  pkcs1,
		BaseURL:        srv.URL,
		Clock:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	src := p.TokenSource("agent-orchestrator", "dev-rag-service")
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ghs_test_installation_token_001" {
		t.Fatalf("token = %q, want ghs_test_installation_token_001", tok)
	}

	// Verify RS256 JWT signature and claims.
	jwt := strings.TrimPrefix(seenAuth, "Bearer ")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed jwt: %q", jwt)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode jwt sig: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("jwt signature verification failed: %v", err)
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Iss != "42" {
		t.Fatalf("iss = %q, want 42", claims.Iss)
	}
	if claims.Exp <= now.Unix() || claims.Exp-now.Unix() > 600 {
		t.Fatalf("unexpected exp %d for now %d", claims.Exp, now.Unix())
	}

	// Verify repository scope was normalized and sent.
	repos, _ := seenBody["repositories"].([]any)
	if len(repos) != 2 || repos[0] != "agent-orchestrator" || repos[1] != "dev-rag-service" {
		t.Fatalf("repositories = %#v, want [agent-orchestrator dev-rag-service]", repos)
	}
}

func TestProvider_CacheExpiryAndInvalidation(t *testing.T) {
	_, pkcs1, _ := generateTestRSAKey(t)
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("ghs_token_%03d_abcdef", n),
			"expires_at": clock().Add(10 * time.Minute).Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	p, err := NewProvider(Config{
		AppID:          7,
		InstallationID: 8,
		PrivateKeyPEM:  pkcs1,
		BaseURL:        srv.URL,
		Clock:          clock,
		RefreshSkew:    2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	src := p.TokenSource("repo-a")
	tok1, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("first Token: %v", err)
	}
	// Cached hit before refresh skew.
	advance(5 * time.Minute)
	tok2, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if tok1 != tok2 || calls.Load() != 1 {
		t.Fatalf("expected cached token (calls=%d, tok1=%q, tok2=%q)", calls.Load(), tok1, tok2)
	}

	// Advance into refresh skew window (8m30s elapsed, 1m30s remaining < 2m skew).
	advance(3*time.Minute + 30*time.Second)
	tok3, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("third Token: %v", err)
	}
	if tok3 == tok1 || calls.Load() != 2 {
		t.Fatalf("expected proactive refresh (calls=%d, tok3=%q)", calls.Load(), tok3)
	}

	// Explicit invalidation forces a new exchange immediately.
	src.InvalidateToken()
	tok4, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("fourth Token: %v", err)
	}
	if tok4 == tok3 || calls.Load() != 3 {
		t.Fatalf("expected refresh after invalidation (calls=%d, tok4=%q)", calls.Load(), tok4)
	}
}

func TestProvider_ConcurrentRefreshDeduplication(t *testing.T) {
	_, pkcs1, _ := generateTestRSAKey(t)
	var calls atomic.Int32
	gate := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-gate
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_deduped_token_123456",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	p, err := NewProvider(Config{
		AppID:          10,
		InstallationID: 20,
		PrivateKeyPEM:  pkcs1,
		BaseURL:        srv.URL,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	tokens := make(chan string, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := p.TokenForRepositories(context.Background(), []string{"repo-x"})
			if err != nil {
				errs <- err
				return
			}
			tokens <- tok
		}()
	}

	// Let goroutines queue up, then release the single HTTP exchange.
	time.Sleep(30 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	close(tokens)

	for err := range errs {
		t.Fatalf("concurrent Token error: %v", err)
	}
	for tok := range tokens {
		if tok != "ghs_deduped_token_123456" {
			t.Fatalf("unexpected token %q", tok)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 HTTP exchange for %d concurrent callers, got %d", workers, got)
	}
}

func TestProvider_ConcurrentRefreshLeaderCancellationDoesNotFailWaiters(t *testing.T) {
	_, pkcs1, _ := generateTestRSAKey(t)
	started := make(chan struct{})
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_waiter_survived_token",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	p, err := NewProvider(Config{
		AppID:          10,
		InstallationID: 20,
		PrivateKeyPEM:  pkcs1,
		BaseURL:        srv.URL,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := p.TokenForRepositories(leaderCtx, []string{"repo-shared"})
		leaderErr <- err
	}()

	<-started

	waiterRes := make(chan string, 1)
	waiterErr := make(chan error, 1)
	go func() {
		tok, err := p.TokenForRepositories(context.Background(), []string{"repo-shared"})
		if err != nil {
			waiterErr <- err
			return
		}
		waiterRes <- tok
	}()

	time.Sleep(20 * time.Millisecond)
	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v, want context.Canceled", err)
	}

	close(release)
	select {
	case err := <-waiterErr:
		t.Fatalf("waiter failed after leader cancellation: %v", err)
	case tok := <-waiterRes:
		if tok != "ghs_waiter_survived_token" {
			t.Fatalf("waiter token = %q, want ghs_waiter_survived_token", tok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for waiter")
	}
}

func TestProvider_RevokedCredentialsAndRedaction(t *testing.T) {
	_, pkcs1, _ := generateTestRSAKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": "Bad credentials ghs_secret1234567890abcdef",
		})
	}))
	defer srv.Close()

	p, err := NewProvider(Config{
		AppID:          11,
		InstallationID: 22,
		PrivateKeyPEM:  pkcs1,
		BaseURL:        srv.URL,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = p.TokenSource().Token(context.Background())
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed, got %v", err)
	}
	if strings.Contains(err.Error(), "ghs_secret1234567890abcdef") {
		t.Fatalf("error leaked secret token: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("expected redacted marker in error: %v", err)
	}

	if strings.Contains(p.String(), "PRIVATE KEY") || strings.Contains(p.GoString(), "PRIVATE KEY") {
		t.Fatalf("String/GoString leaked private key")
	}

	// Malformed or owner-qualified repository scopes must fail closed.
	for _, bad := range []string{"owner/repo", "owner/", "/repo", "owner/repo/extra", " "} {
		if _, err := p.TokenForRepositories(context.Background(), []string{bad}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("malformed repo scope %q: got %v, want ErrInvalidConfig", bad, err)
		}
	}
}
