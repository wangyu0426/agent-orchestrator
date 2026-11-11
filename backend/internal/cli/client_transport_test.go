package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// transportServer returns an httptest server that always answers with the
// given status and body for any CLI daemon call.
func transportServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func transportContext(t *testing.T, cfg testConfig, srv *httptest.Server) *commandContext {
	t.Helper()
	writeRunFileFor(t, cfg, srv)
	return &commandContext{deps: Deps{
		HTTPClient:   &http.Client{},
		ProcessAlive: func(int) bool { return true },
	}}
}

// TestTransportDistinguishesEmptyResponses is the API-014 regression: an empty
// 200/201 with a required decoded result must fail, while legitimate
// bodyless calls (nil out) and 204 responses keep succeeding.
func TestTransportDistinguishesEmptyResponses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("empty 200 with required body fails", func(t *testing.T) {
		cfg := setConfigEnv(t)
		srv := transportServer(t, http.StatusOK, ``)
		c := transportContext(t, cfg, srv)
		var out spawnResult
		if err := c.postJSON(ctx, "sessions", struct{}{}, &out); err == nil {
			t.Fatal("expected error for empty 200 with required body, got nil")
		} else if !strings.Contains(err.Error(), "missing required response body") {
			t.Fatalf("error = %q, want missing required response body", err.Error())
		}
	})

	t.Run("empty 201 with required body fails", func(t *testing.T) {
		cfg := setConfigEnv(t)
		srv := transportServer(t, http.StatusCreated, ``)
		c := transportContext(t, cfg, srv)
		var out spawnResult
		if err := c.postJSON(ctx, "sessions", struct{}{}, &out); err == nil {
			t.Fatal("expected error for empty 201 with required body, got nil")
		}
	})

	t.Run("204 empty with decoded result succeeds", func(t *testing.T) {
		cfg := setConfigEnv(t)
		srv := transportServer(t, http.StatusNoContent, ``)
		c := transportContext(t, cfg, srv)
		var out projectRemoveResult
		if err := c.deleteJSON(ctx, "projects/demo", &out); err != nil {
			t.Fatalf("204 with decoded result should succeed, got: %v", err)
		}
	})

	t.Run("nil output tolerates empty 200", func(t *testing.T) {
		cfg := setConfigEnv(t)
		srv := transportServer(t, http.StatusOK, ``)
		c := transportContext(t, cfg, srv)
		if err := c.postJSON(ctx, "sessions/ao-1/activity", struct{}{}, nil); err != nil {
			t.Fatalf("nil output should tolerate empty body, got: %v", err)
		}
	})

	t.Run("valid JSON still decodes", func(t *testing.T) {
		cfg := setConfigEnv(t)
		srv := transportServer(t, http.StatusOK, `{"session":{"id":"demo-11","status":"idle"}}`)
		c := transportContext(t, cfg, srv)
		var out spawnResult
		if err := c.postJSON(ctx, "sessions", struct{}{}, &out); err != nil {
			t.Fatalf("valid JSON should decode, got: %v", err)
		}
		if out.Session.ID != "demo-11" {
			t.Fatalf("Session.ID = %q, want demo-11", out.Session.ID)
		}
	})

	t.Run("malformed JSON fails", func(t *testing.T) {
		cfg := setConfigEnv(t)
		srv := transportServer(t, http.StatusOK, `{not-json`)
		c := transportContext(t, cfg, srv)
		var out spawnResult
		if err := c.postJSON(ctx, "sessions", struct{}{}, &out); err == nil {
			t.Fatal("expected error for malformed JSON, got nil")
		}
	})

	t.Run("literal null with required body fails", func(t *testing.T) {
		for _, body := range []string{`null`, "  null  \n"} {
			cfg := setConfigEnv(t)
			srv := transportServer(t, http.StatusOK, body)
			c := transportContext(t, cfg, srv)
			var out spawnResult
			err := c.postJSON(ctx, "sessions", struct{}{}, &out)
			if err == nil {
				t.Fatalf("body %q: expected error for null body, got nil", body)
			}
			if !strings.Contains(err.Error(), "missing required response body") {
				t.Fatalf("body %q: error = %q, want missing required response body", body, err.Error())
			}
		}
	})

	t.Run("daemon error envelope keeps code and request id", func(t *testing.T) {
		cfg := setConfigEnv(t)
		srv := transportServer(t, http.StatusNotFound, `{"message":"nope","code":"NOT_FOUND","requestId":"req-1"}`)
		c := transportContext(t, cfg, srv)
		var out spawnResult
		err := c.postJSON(ctx, "sessions", struct{}{}, &out)
		var apiErr apiResponseError
		if !errors.As(err, &apiErr) {
			t.Fatalf("error type = %T, want apiResponseError", err)
		}
		if apiErr.ErrorBody.Code != "NOT_FOUND" || apiErr.ErrorBody.RequestID != "req-1" {
			t.Fatalf("envelope = %+v, want code NOT_FOUND request req-1", apiErr.ErrorBody)
		}
	})
}

// TestSpawnRejectsEmptySessionID ensures a daemon that answers spawn with an
// empty (but valid) body can never print a success line.
func TestSpawnRejectsEmptySessionID(t *testing.T) {
	for _, body := range []string{``, `{}`} {
		cfg := setConfigEnv(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/demo":
				_, _ = io.WriteString(w, `{"status":"ok","project":{"id":"demo","name":"Demo","path":"/repo/demo","config":{"worker":{"agent":"codex"}}}}`)
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/readiness/ensure":
				_, _ = io.WriteString(w, authorizedAgentsJSON("codex"))
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions":
				_, _ = io.WriteString(w, body)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		writeRunFileFor(t, cfg, srv)

		out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
			"spawn", "--project", "demo", "--agent", "codex", "--name", "worker")
		if err == nil {
			t.Fatalf("body %q: expected spawn error, got success %q", body, out)
		}
		if strings.Contains(out, "spawned session") {
			t.Fatalf("body %q: must not print success, got %q", body, out)
		}
	}
}

// TestReviewSubmitBatchRejectsEmptyEntries ensures batch review submit fails
// instead of printing success when any returned entry is missing its run ID
// or verdict — including empty objects and JSON nulls.
func TestReviewSubmitBatchRejectsEmptyEntries(t *testing.T) {
	batchInput := `[{"runId":"run-1","verdict":"approved"}]`
	tests := []struct {
		name string
		body string
	}{
		{"empty entry", `{"reviews":[{}]}`},
		{"null entry", `{"reviews":[null]}`},
		{"id-only entry", `{"reviews":[{"id":"run-1"}]}`},
		{"verdict-only entry", `{"reviews":[{"verdict":"approved"}]}`},
		{"one bad entry among good", `{"reviews":[{"id":"run-1","verdict":"approved"},{}]}`},
		{"empty array and empty single", `{"reviews":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)
			writeRunFileFor(t, cfg, srv)

			deps := Deps{ProcessAlive: func(int) bool { return true }}
			deps.In = strings.NewReader(batchInput)
			out, _, err := executeCLI(t, deps,
				"review", "submit", "sess-1", "--reviews", "-")
			if err == nil {
				t.Fatalf("body %q: expected batch submit error, got nil", tt.body)
			}
			if !strings.Contains(err.Error(), "empty review result") {
				t.Fatalf("body %q: err = %q, want empty review result", tt.body, err.Error())
			}
			if strings.Contains(out, "recorded") {
				t.Fatalf("body %q: must not print success, got %q", tt.body, out)
			}
		})
	}
}

// TestReviewSubmitRejectsEmptyResult ensures review submit fails instead of
// reporting a recorded review when the daemon returns nothing useful: an
// empty body fails at the transport layer, while a valid-but-empty document
// fails at the caller guard.
func TestReviewSubmitRejectsEmptyResult(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"empty body fails at transport", ``, "missing required response body"},
		{"empty document fails at guard", `{}`, "empty review result"},
		{"half-populated document fails at guard", `{"review":{"id":"run-1"}}`, "empty review result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)
			writeRunFileFor(t, cfg, srv)

			_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
				"review", "submit", "sess-1", "--run", "run-1", "--verdict", "approved")
			if err == nil {
				t.Fatalf("body %q: expected review submit error, got nil", tt.body)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("body %q: err = %q, want %q", tt.body, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestTransportHealthFallbackForSandboxInvisibleDaemonPID(t *testing.T) {
	const daemonPID = 254414
	for _, tc := range []struct {
		name        string
		healthBody  string
		healthCode  int
		wantSuccess bool
	}{
		{
			name: "matching daemon service and PID",
			healthBody: `{"service":"agent-orchestrator-daemon","pid":254414}`,
			healthCode: http.StatusOK,
			wantSuccess: true,
		},
		{
			name: "wrong PID does not establish ownership",
			healthBody: `{"service":"agent-orchestrator-daemon","pid":123}`,
			healthCode: http.StatusOK,
		},
		{
			name: "wrong service does not establish ownership",
			healthBody: `{"service":"unrelated-service","pid":254414}`,
			healthCode: http.StatusOK,
		},
		{
			name: "invalid health payload remains stale",
			healthBody: `{bad-json`,
			healthCode: http.StatusOK,
		},
		{
			name: "unhealthy endpoint remains stale",
			healthBody: `{"service":"agent-orchestrator-daemon","pid":254414}`,
			healthCode: http.StatusServiceUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			var apiRequests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/healthz" {
					w.WriteHeader(tc.healthCode)
					_, _ = io.WriteString(w, tc.healthBody)
					return
				}
				if r.URL.Path == "/api/v1/projects/demo" {
					apiRequests.Add(1)
					_, _ = io.WriteString(w, `{"status":"ok"}`)
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(srv.Close)
			if err := runfile.Write(cfg.runFile, runfile.Info{PID: daemonPID, Port: serverPort(t, srv.URL)}); err != nil {
				t.Fatal(err)
			}
			c := &commandContext{deps: Deps{
				HTTPClient: &http.Client{Timeout: 2 * time.Second},
				ProcessAlive: func(int) bool { return false },
			}}
			var out map[string]any
			err := c.getJSON(context.Background(), "projects/demo", &out)
			if tc.wantSuccess {
				if err != nil || out["status"] != "ok" || apiRequests.Load() != 1 {
					t.Fatalf("getJSON = %v out=%v apiRequests=%d; want successful API request", err, out, apiRequests.Load())
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "stale run-file") || apiRequests.Load() != 0 {
					t.Fatalf("getJSON = %v apiRequests=%d; want stale rejection before API call", err, apiRequests.Load())
				}
			}
		})
	}
}

func TestTransportHealthFallbackDoesNotProbeWhenPIDVisible(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			t.Error("health fallback must not run when process exists")
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	t.Cleanup(srv.Close)
	if err := runfile.Write(cfg.runFile, runfile.Info{PID: 254414, Port: serverPort(t, srv.URL)}); err != nil {
		t.Fatal(err)
	}
	c := &commandContext{deps: Deps{
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
		ProcessAlive: func(int) bool { return true },
	}}
	var out map[string]any
	if err := c.getJSON(context.Background(), "projects/demo", &out); err != nil {
		t.Fatal(err)
	}
}

func TestTransportExplicitLoopbackProxyForInvisiblePID(t *testing.T) {
	cfg := setConfigEnv(t)
	t.Setenv("AO_CLI_PROXY_LOOPBACK", "1")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")

	var healthCount, apiCount atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "127.0.0.1:3301" {
			t.Errorf("proxy received unexpected target %q", r.URL.Host)
			http.Error(w, "wrong host", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			healthCount.Add(1)
			_, _ = io.WriteString(w, `{"service":"agent-orchestrator-daemon","pid":254414}`)
		case "/api/v1/projects/demo":
			apiCount.Add(1)
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("http_proxy", proxy.URL)
	if err := runfile.Write(cfg.runFile, runfile.Info{PID: 254414, Port: 3301}); err != nil {
		t.Fatal(err)
	}
	c := &commandContext{deps: Deps{
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
		ProcessAlive: func(int) bool { return false },
	}}
	var out map[string]any
	if err := c.getJSON(context.Background(), "projects/demo", &out); err != nil {
		t.Fatalf("proxied CLI request failed: %v", err)
	}
	if out["status"] != "ok" || healthCount.Load() != 1 || apiCount.Load() != 1 {
		t.Fatalf("status=%v health=%d api=%d; want ok / 1 / 1", out["status"], healthCount.Load(), apiCount.Load())
	}
}

func TestTransportExplicitLoopbackProxyRejectsRemoteEndpoints(t *testing.T) {
	t.Setenv("AO_CLI_PROXY_LOOPBACK", "1")
	t.Setenv("HTTP_PROXY", "")
	for _, value := range []string{
		"http://example.com:3128",
		"https://127.0.0.1:3128",
		"http://person:pass@127.0.0.1:3128",
		"http://127.0.0.1",
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("http_proxy", value)
			c := &commandContext{deps: Deps{HTTPClient: &http.Client{}}}
			if _, err := c.daemonHTTPClient(); err == nil {
				t.Fatalf("proxy %q was accepted, want rejection", value)
			}
		})
	}
}


// TestTransportProxyReviewSubmitPOSTAndReadBack covers the actual reviewer CLI
// request path (not just a read-only GET). The stand-in proxy implements a
// minimal stateful daemon, so no real GitHub PR or daemon is mutated. The
// persisted-daemon and GitHub review E2E remain separate acceptance checks.
func TestTransportProxyReviewSubmitPOSTAndReadBack(t *testing.T) {
	cfg := setReviewEnv(t)
	t.Setenv("AO_CLI_PROXY_LOOPBACK", "1")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")

	const worker = "scratch-worker"
	const runID = "scratch-review-run"
	const bodyText = "isolated E2E transport test"
	var healthCount, telemetryCount, submitCount, readCount atomic.Int32
	var recorded bool
	var firstBody string
	var stateMu sync.Mutex
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "127.0.0.1:3301" {
			t.Errorf("unexpected proxy request target %q", r.URL.Host)
			http.Error(w, "unexpected target", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			healthCount.Add(1)
			_, _ = io.WriteString(w, `{"service":"agent-orchestrator-daemon","pid":254414}`)
		case r.Method == http.MethodPost && r.URL.Path == "/internal/telemetry/cli-invoked":
			// Each CLI command reports best-effort usage before its API call.
			// Both requests perform independent PID-invisible health probes.
			telemetryCount.Add(1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/"+worker+"/reviews/submit":
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				http.Error(w, "read body", http.StatusBadRequest)
				return
			}
			reqBody := string(raw)
			submitCount.Add(1)
			stateMu.Lock()
			defer stateMu.Unlock()
			if recorded && firstBody != reqBody {
				http.Error(w, `{"message":"conflicting submission","code":"INVALID"}`, http.StatusConflict)
				return
			}
			if !recorded {
				firstBody = reqBody
				recorded = true
			}
			var submitted submitReviewRequest
			if err := json.Unmarshal(raw, &submitted); err != nil ||
				submitted.RunID != runID ||
				submitted.Verdict != "changes_requested" ||
				submitted.Body != bodyText ||
				submitted.GithubReviewID != "12345" {
				t.Errorf("incorrect review request: %+v decode err=%v", submitted, err)
				http.Error(w, "invalid review", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"review":{"id":"scratch-review-run","verdict":"changes_requested","githubReviewId":"12345"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/"+worker+"/reviews":
			readCount.Add(1)
			stateMu.Lock()
			defer stateMu.Unlock()
			if !recorded {
				http.Error(w, "no submitted review", http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, `{"reviews":[{"prNumber":1,"targetSha":"scratch-head","status":"up_to_date","latestRun":{"id":"scratch-review-run","verdict":"changes_requested","githubReviewId":"12345"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("http_proxy", proxy.URL)
	if err := runfile.Write(cfg.runFile, runfile.Info{PID: 254414, Port: 3301}); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		var out, errOut strings.Builder
		deps := Deps{
			HTTPClient:   &http.Client{Timeout: 2 * time.Second},
			ProcessAlive: func(int) bool { return false },
			In:           strings.NewReader(bodyText),
			Out:          &out,
			Err:          &errOut,
		}
		cmd := NewRootCommand(deps)
		cmd.SetArgs([]string{
			"review", "submit", worker, "--run", runID, "--verdict", "changes_requested",
			"--body", "-", "--review-id", "12345",
		})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("attempt %d submit failed: %v stderr=%s", attempt+1, err, errOut.String())
		}
		if !strings.Contains(out.String(), "recorded changes_requested review") {
			t.Fatalf("attempt %d reported no successful submission: %q", attempt+1, out.String())
		}
	}
	out, _, err := executeCLI(t, Deps{
		HTTPClient:   &http.Client{Timeout: 2 * time.Second},
		ProcessAlive: func(int) bool { return false },
	}, "review", "ls", worker, "--json")
	if err != nil {
		t.Fatalf("read-back through proxy failed: %v", err)
	}
	if !strings.Contains(out, `"githubReviewId": "12345"`) ||
		!strings.Contains(out, `"verdict": "changes_requested"`) {
		t.Fatalf("read-back did not preserve review result: %s", out)
	}
	// Three CLI invocations each emit one telemetry POST and one API call.
	// When the host PID is hidden, both operations validate /healthz first.
	if healthCount.Load() != 6 || telemetryCount.Load() != 3 || submitCount.Load() != 2 || readCount.Load() != 1 {
		t.Fatalf("unexpected proxy calls health=%d telemetry=%d submit=%d read=%d",
			healthCount.Load(), telemetryCount.Load(), submitCount.Load(), readCount.Load())
	}
}
