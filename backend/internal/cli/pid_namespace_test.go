package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// Workers in an isolated PID namespace may not see the host daemon PID, even
// when its loopback API is reachable. Identity-checked HTTP is authoritative.
func TestDaemonStatusAndCallsWhenHostPIDInvisible(t *testing.T) {
	cfg := setConfigEnv(t)
	const pid = 424242
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_, _ = fmt.Fprintf(w, `{"status":"ok","service":%q,"pid":%d}`, daemonmeta.ServiceName, pid)
		case "/readyz":
			_, _ = fmt.Fprintf(w, `{"status":"ready","service":%q,"pid":%d}`, daemonmeta.ServiceName, pid)
		case "/api/v1/projects":
			_, _ = w.Write([]byte(`{"projects":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if err := runfile.Write(cfg.runFile, runfile.Info{
		PID: pid, Port: serverPort(t, srv.URL), StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	c := &commandContext{deps: (Deps{
		HTTPClient:   srv.Client(),
		ProcessAlive: func(int) bool { return false },
	}).withDefaults()}

	st, err := c.inspectDaemon(context.Background())
	if err != nil || st.State != stateReady {
		t.Fatalf("status = %+v, err = %v; want ready", st, err)
	}
	var got map[string]any
	if err := c.getJSON(context.Background(), "projects", &got); err != nil {
		t.Fatalf("API call rejected verified daemon with invisible PID: %v", err)
	}
}

// A process-invisible client must not contact API routes if /healthz has the
// wrong service or PID. This also protects against PID reuse / port takeover.
func TestInvisibleDaemonPIDRequiresMatchingHealthIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service string
		pid     int
	}{
		{name: "wrong service", service: "not-orchestrator", pid: 424242},
		{name: "wrong pid", service: daemonmeta.ServiceName, pid: 424243},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/healthz":
					_, _ = fmt.Fprintf(w, `{"status":"ok","service":%q,"pid":%d}`, tc.service, tc.pid)
				case "/readyz":
					_, _ = fmt.Fprintf(w, `{"status":"ready","service":%q,"pid":%d}`, tc.service, tc.pid)
				default:
					t.Errorf("unexpected API request to %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			if err := runfile.Write(cfg.runFile, runfile.Info{
				PID: 424242, Port: serverPort(t, srv.URL), StartedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
			c := &commandContext{deps: (Deps{
				HTTPClient:   srv.Client(),
				ProcessAlive: func(int) bool { return false },
			}).withDefaults()}
			st, err := c.inspectDaemon(context.Background())
			if err != nil || st.State != stateStale {
				t.Fatalf("status = %+v, err = %v; want stale", st, err)
			}
			var got map[string]any
			err = c.getJSON(context.Background(), "projects", &got)
			if !errors.Is(err, errDaemonNotRunning) {
				t.Fatalf("API call = %v; want daemon-not-running", err)
			}
		})
	}
}
