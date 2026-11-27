package httpd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubapp"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubbroker"
)

type stubTokenMinter struct {
	token     string
	expiresAt time.Time
	invalided int
}

func (s *stubTokenMinter) MintScopedToken(_ context.Context, req githubapp.ScopedTokenRequest) (githubapp.ScopedToken, error) {
	return githubapp.ScopedToken{
		Token:        s.token,
		ExpiresAt:    s.expiresAt,
		Repositories: req.Repositories,
		Permissions:  req.Permissions,
	}, nil
}

func (s *stubTokenMinter) InvalidateScopedToken(_ githubapp.ScopedTokenRequest) {
	s.invalided++
}

func TestMountGitHubBroker_LoopbackAuthAndFailClosed(t *testing.T) {
	minter := &stubTokenMinter{
		token:     "ghs_http_broker_token_1234567890",
		expiresAt: time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC),
	}
	broker, err := githubbroker.New(githubbroker.Config{
		Provider:     minter,
		AllowedRepos: []string{"wangyu0426/agent-orchestrator"},
	})
	if err != nil {
		t.Fatalf("githubbroker.New: %v", err)
	}
	sessToken, err := broker.RegisterSession("sess-1", "proj-1", []string{"wangyu0426/agent-orchestrator"})
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}

	r := chi.NewRouter()
	mountGitHubBroker(r, broker)

	// Happy path on loopback.
	body := bytes.NewBufferString(`{"session_id":"sess-1","host":"github.com","repository":"wangyu0426/agent-orchestrator"}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/internal/github/broker/token", body)
	req.Host = "127.0.0.1:4242"
	req.Header.Set("Authorization", "Bearer "+sessToken)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("expected Cache-Control: no-store, got %q", got)
	}
	var resp githubbroker.IssueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Token != minter.token || resp.Username != githubbroker.GitUsername {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// Missing bearer token -> 401.
	req2 := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/internal/github/broker/token", bytes.NewBufferString(`{"session_id":"sess-1"}`))
	req2.Host = "127.0.0.1:4242"
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer token, got %d", rec2.Code)
	}

	// Unauthorized repo -> 403.
	req3 := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/internal/github/broker/token", bytes.NewBufferString(`{"session_id":"sess-1","repository":"other/repo"}`))
	req3.Host = "127.0.0.1:4242"
	req3.Header.Set("Authorization", "Bearer "+sessToken)
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unauthorized repo, got %d", rec3.Code)
	}

	// Invalidate endpoint -> 204.
	req4 := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4242/internal/github/broker/invalidate", bytes.NewBufferString(`{"session_id":"sess-1","repository":"wangyu0426/agent-orchestrator"}`))
	req4.Host = "127.0.0.1:4242"
	req4.Header.Set("Authorization", "Bearer "+sessToken)
	rec4 := httptest.NewRecorder()
	r.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusNoContent || minter.invalided != 1 {
		t.Fatalf("expected 204 and invalided=1, got %d and %d", rec4.Code, minter.invalided)
	}

	// LAN listener must block /internal/github/broker/token with 404.
	lanHandler := lanControlBlock(r)
	lanReq := httptest.NewRequest(http.MethodPost, "http://192.168.1.5:4242/internal/github/broker/token", bytes.NewBufferString(`{"session_id":"sess-1"}`))
	lanReq.Host = "192.168.1.5:4242"
	lanReq.Header.Set("Authorization", "Bearer "+sessToken)
	lanRec := httptest.NewRecorder()
	lanHandler.ServeHTTP(lanRec, lanReq)
	if lanRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on LAN listener, got %d", lanRec.Code)
	}
}
