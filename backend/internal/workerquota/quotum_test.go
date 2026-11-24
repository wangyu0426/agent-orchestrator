package workerquota

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestQuotumUsesAuthenticatedOverviewAndSharedWindows(t *testing.T) {
	now := time.Now()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		cookie, err := r.Cookie("quotum_session")
		if r.Method != http.MethodGet || r.URL.Path != "/api/overview" || r.URL.Query().Get("board") != "board-id" ||
			err != nil || cookie.Value != "test-cookie" {
			t.Errorf("unexpected Quotum request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"sources":[
			{"id":"other-account","provider":"antigravity","stale":false,"successAt":%d,"staleAfterMs":60000,
				"windows":[{"id":"weekly","label":null,"used":100,"resetAt":%d}]},
			{"id":"codex-source","provider":"codex","stale":false,"error":null,"successAt":%d,"staleAfterMs":60000,
				"windows":[{"id":"weekly","label":null,"used":100,"resetAt":%d}]},
			{"id":"agy-source","provider":"antigravity","stale":false,"successAt":%d,"staleAfterMs":60000,
				"windows":[{"id":"gemini:weekly","label":"Gemini","used":100,"resetAt":%d},
				           {"id":"3p:weekly","label":"Third Party","used":30,"resetAt":null}]}]}`,
			now.UnixMilli(), now.Add(time.Hour).UnixMilli(), now.UnixMilli(), now.Add(time.Hour).UnixMilli(),
			now.UnixMilli(), now.Add(time.Hour).UnixMilli())
	}))
	t.Cleanup(server.Close)
	q, err := NewQuotum(server.URL, "board-id", "test-cookie", map[domain.AgentHarness]Binding{
		domain.HarnessCodex: {SourceID: "codex-source"},
		domain.HarnessAgy: {SourceID: "agy-source", ModelGroups: map[string][]string{
			"gemini-pro": {"gemini:weekly"}, "claude": {"3p:weekly"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	targets := []Target{{Harness: domain.HarnessCodex, Model: "a"}, {Harness: domain.HarnessCodex, Model: "b"},
		{Harness: domain.HarnessAgy, Model: "gemini-pro"}, {Harness: domain.HarnessAgy, Model: "claude"}}
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			windows, err := q.Read(context.Background(), targets)
			target, ok := Select(targets, windows, now)
			if err != nil || !ok || target != targets[3] {
				t.Errorf("selected %+v, %v, %v", target, ok, err)
			}
		})
	}
	readers.Wait()
	if requests.Load() != 1 {
		t.Fatalf("snapshot requested %d times", requests.Load())
	}
}

func TestQuotumRejectsRedirectAndKeepsAuthFailuresUnknown(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "/sign-in", http.StatusFound)
	}))
	t.Cleanup(server.Close)
	q, err := NewQuotum(server.URL, "", "test-cookie", nil)
	if err != nil {
		t.Fatal(err)
	}
	if windows, err := q.Read(context.Background(), nil); err == nil || windows != nil || requests.Load() != 1 {
		t.Fatalf("redirect read = %v, %v, requests=%v", windows, err, requests.Load())
	}
}
