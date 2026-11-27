package daemon

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	scmgithub "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/github"
	scmmulti "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/multi"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	scmobserve "github.com/aoagents/agent-orchestrator/backend/internal/observe/scm"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type fakeFeedbackMessenger struct {
	mu       sync.Mutex
	messages []deliveredFeedback
	err      error
}

func (m *fakeFeedbackMessenger) Send(_ context.Context, sessionID domain.SessionID, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.messages = append(m.messages, deliveredFeedback{sessionID: sessionID, message: message})
	return nil
}

func (m *fakeFeedbackMessenger) SendUrgent(ctx context.Context, sessionID domain.SessionID, message string) error {
	return m.Send(ctx, sessionID, message)
}

func (m *fakeFeedbackMessenger) snapshot() []deliveredFeedback {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]deliveredFeedback(nil), m.messages...)
}

type mockCommentNode struct {
	ID        string
	Author    string
	TypeName  string // "User" or "Bot"
	Body      string
	URL       string
	CreatedAt string
	UpdatedAt string
}

type mockPRServer struct {
	mu           sync.Mutex
	prState      string
	merged       bool
	closed       bool
	headSHA      string
	prAuthor     string
	commentPages [][]mockCommentNode
	server       *httptest.Server
}

func newMockPRServer(t *testing.T, prAuthor, headSHA string) *mockPRServer {
	t.Helper()
	m := &mockPRServer{
		prState:  "OPEN",
		headSHA:  headSHA,
		prAuthor: prAuthor,
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"ghs_app_token_test","expires_at":"2099-01-01T00:00:00Z"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls"):
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"etag-pulls-1"`)
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"etag-checks-1"`)
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			bodyBytes, _ := io.ReadAll(r.Body)
			var payload struct {
				Query string `json:"query"`
			}
			_ = json.Unmarshal(bodyBytes, &payload)
			w.Header().Set("Content-Type", "application/json")

			m.mu.Lock()
			prState := m.prState
			merged := m.merged
			closed := m.closed
			headSHA := m.headSHA
			prAuthor := m.prAuthor
			pages := m.commentPages
			m.mu.Unlock()

			// Paginated conversation comments query.
			if strings.Contains(payload.Query, "comments(first:") {
				pageIdx := 0
				if strings.Contains(payload.Query, `after:"cursor-page-1"`) {
					pageIdx = 1
				}
				var nodes []map[string]any
				hasNext := false
				endCursor := ""
				if pageIdx < len(pages) {
					for _, c := range pages[pageIdx] {
						typ := c.TypeName
						if typ == "" {
							typ = "User"
						}
						nodes = append(nodes, map[string]any{
							"id":        c.ID,
							"body":      c.Body,
							"url":       c.URL,
							"createdAt": c.CreatedAt,
							"updatedAt": c.UpdatedAt,
							"author": map[string]any{
								"login":      c.Author,
								"__typename": typ,
							},
						})
					}
					if pageIdx+1 < len(pages) {
						hasNext = true
						endCursor = fmt.Sprintf("cursor-page-%d", pageIdx+1)
					}
				}
				resp := map[string]any{
					"data": map[string]any{
						"repo": map[string]any{
							"pullRequest": map[string]any{
								"comments": map[string]any{
									"nodes": nodes,
									"pageInfo": map[string]any{
										"hasNextPage": hasNext,
										"endCursor":   endCursor,
									},
								},
							},
						},
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
				return
			}

			// Standard batch PR / review threads query.
			prNode := fmt.Sprintf(`{
				"id": "PR_webpob_9",
				"number": 9,
				"url": "https://github.com/wangyu0426/WebPoB/pull/9",
				"state": %q,
				"isDraft": false,
				"merged": %t,
				"closed": %t,
				"title": "WebPoB PR #9",
				"mergeable": "MERGEABLE",
				"mergeStateStatus": "CLEAN",
				"reviewDecision": "REVIEW_REQUIRED",
				"baseRefName": "main",
				"headRefName": "feat/webpob-9",
				"headRefOid": %q,
				"updatedAt": "2026-11-27T17:22:00Z",
				"author": {"__typename": "User", "login": %q},
				"reviewSummaries": {"nodes": []},
				"reviewThreads": {"nodes": []},
				"commits": {"nodes": [{"commit": {"oid": %q, "statusCheckRollup": null}}]}
			}`, prState, merged, closed, headSHA, prAuthor, headSHA)
			_, _ = fmt.Fprintf(w, `{"data":{"pr0":{"pullRequest":%s},"repo":{"pullRequest":%s}}}`, prNode, prNode)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockPRServer) setComments(pages ...[]mockCommentNode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commentPages = pages
}

func (m *mockPRServer) setPRState(state string, merged, closed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prState = state
	m.merged = merged
	m.closed = closed
}

func generateTestRSAKeyFile(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}

func TestPRCommentFeedback_WebPoB9_AppAndLegacyAuth_UnchangedHeadAndSameAuthor(t *testing.T) {
	for _, mode := range []string{"github_app", "legacy_token"} {
		t.Run(mode, func(t *testing.T) {
			mockSrv := newMockPRServer(t, "wangyu0426", "deadbeef9999")
			mockSrv.setComments([]mockCommentNode{
				{
					ID:        "IC_bot_1",
					Author:    "ao-daemon[bot]",
					TypeName:  "Bot",
					Body:      "/ao review-feedback bot spoofed status",
					URL:       "https://github.com/wangyu0426/WebPoB/pull/9#issuecomment-1",
					CreatedAt: "2026-11-27T17:20:00Z",
				},
				{
					ID:        "IC_unmarked",
					Author:    "wangyu0426",
					TypeName:  "User",
					Body:      "Just a regular conversation comment without marker",
					URL:       "https://github.com/wangyu0426/WebPoB/pull/9#issuecomment-2",
					CreatedAt: "2026-11-27T17:21:00Z",
				},
				{
					ID:        "IC_spoofed",
					Author:    "wangyu0426",
					TypeName:  "User",
					Body:      "> /ao review-feedback quoted marker must be ignored",
					URL:       "https://github.com/wangyu0426/WebPoB/pull/9#issuecomment-3",
					CreatedAt: "2026-11-27T17:21:30Z",
				},
				{
					ID:        "IC_untrusted",
					Author:    "random-collaborator",
					TypeName:  "User",
					Body:      "/ao review-feedback untrusted author must be rejected",
					URL:       "https://github.com/wangyu0426/WebPoB/pull/9#issuecomment-4",
					CreatedAt: "2026-11-27T17:21:45Z",
				},
				{
					ID:        "IC_7224137352",
					Author:    "wangyu0426", // Same as PR author (shared account via connector)
					TypeName:  "User",
					Body:      "/ao review-feedback\nFinal-gate feedback: add nil-check on item parser and rerun tests.",
					URL:       "https://github.com/wangyu0426/WebPoB/pull/9#issuecomment-7224137352",
					CreatedAt: "2026-11-27T17:22:00Z",
				},
			})

			st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatalf("sqlite.Open: %v", err)
			}
			defer func() { _ = st.Close() }()

			now := time.Date(2026, 11, 27, 17, 25, 0, 0, time.UTC)
			if err := st.UpsertProject(context.Background(), domain.ProjectRecord{
				ID:            "webpob",
				Path:          t.TempDir(),
				RepoOriginURL: "https://github.com/wangyu0426/WebPoB.git",
				RegisteredAt:  now,
				Config: domain.ProjectConfig{
					PRCommentFeedback: domain.PRCommentFeedbackConfig{
						Enabled:        true,
						TrustedAuthors: []string{"wangyu0426"},
					},
				},
			}); err != nil {
				t.Fatalf("UpsertProject: %v", err)
			}

			rec, err := st.CreateSession(context.Background(), domain.SessionRecord{
				ProjectID:        "webpob",
				Kind:             domain.KindWorker,
				Metadata:         domain.SessionMetadata{Branch: "feat/webpob-9"},
				Activity:         domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
				AutoInjectReview: true,
			})
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}

			prURL := "https://github.com/wangyu0426/WebPoB/pull/9"
			if _, err := st.ClaimPR(context.Background(), domain.PullRequest{
				URL:       prURL,
				SessionID: rec.ID,
				Number:    9,
				HeadSHA:   "deadbeef9999",
				Author:    "wangyu0426",
				Provider:  "github",
				Host:      "github.com",
				Repo:      "wangyu0426/WebPoB",
				UpdatedAt: now,
			}, nil, nil, nil, nil, ports.ReviewWriteReplace, true); err != nil {
				t.Fatalf("ClaimPR: %v", err)
			}

			var scmProv *scmgithub.Provider
			if mode == "github_app" {
				auth, err := newGitHubAuthWiring(config.GitHubAppConfig{
					Enabled:        true,
					AppID:          555,
					InstallationID: 777,
					PrivateKeyPath: generateTestRSAKeyFile(t),
					APIBaseURL:     mockSrv.server.URL,
					Repositories:   []string{"WebPoB"},
					OperatorLogin:  "wangyu0426",
				}, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err != nil {
					t.Fatalf("newGitHubAuthWiring: %v", err)
				}
				scmProv, err = newGitHubSCMProviderWithAuth(auth, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err != nil {
					t.Fatalf("newGitHubSCMProviderWithAuth: %v", err)
				}
			} else {
				var err error
				scmProv, err = scmgithub.NewProvider(scmgithub.ProviderOptions{
					RESTBase:   mockSrv.server.URL,
					GraphQLURL: mockSrv.server.URL + "/graphql",
					Token:      scmgithub.StaticTokenSource("ghp_legacy_test_token"),
					Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
				})
				if err != nil {
					t.Fatalf("scmgithub.NewProvider: %v", err)
				}
			}

			messenger := &fakeFeedbackMessenger{}
			lcm := lifecycle.New(st, messenger)
			multi := scmmulti.New(scmmulti.NamedProvider{Key: "github", Provider: scmProv})
			obs := scmobserve.New(multi, st, lcm, scmobserve.Config{
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				Clock:  func() time.Time { return now },
			})

			if err := obs.Poll(context.Background()); err != nil {
				t.Fatalf("obs.Poll: %v", err)
			}

			msgs := messenger.snapshot()
			if len(msgs) != 1 {
				t.Fatalf("expected exactly 1 delivered feedback message, got %d: %+v", len(msgs), msgs)
			}
			if msgs[0].sessionID != rec.ID {
				t.Fatalf("delivered to session %q, want %q", msgs[0].sessionID, rec.ID)
			}
			if !strings.Contains(msgs[0].message, "Final-gate feedback: add nil-check on item parser") {
				t.Fatalf("unexpected delivered message: %q", msgs[0].message)
			}
			if strings.Contains(msgs[0].message, "bot spoofed") || strings.Contains(msgs[0].message, "untrusted author") {
				t.Fatalf("rejected comments leaked into delivered message: %q", msgs[0].message)
			}

			// Verify dedup across second poll AND simulated daemon restart.
			now = now.Add(time.Minute)
			if err := obs.Poll(context.Background()); err != nil {
				t.Fatalf("second obs.Poll: %v", err)
			}
			restartedLcm := lifecycle.New(st, messenger)
			restartedObs := scmobserve.New(multi, st, restartedLcm, scmobserve.Config{
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				Clock:  func() time.Time { return now.Add(time.Minute) },
			})
			if err := restartedObs.Poll(context.Background()); err != nil {
				t.Fatalf("restarted obs.Poll: %v", err)
			}
			if got := len(messenger.snapshot()); got != 1 {
				t.Fatalf("expected deduplication across poll and restart, got %d messages", got)
			}
		})
	}
}

func TestPRCommentFeedback_Pagination_Edit_Supersession_Delete_And_BackoffRecovery(t *testing.T) {
	mockSrv := newMockPRServer(t, "wangyu0426", "deadbeef9999")
	// Put the feedback command on page 2 (beyond page 1's view window).
	mockSrv.setComments(
		[]mockCommentNode{
			{ID: "IC_p1_1", Author: "wangyu0426", Body: "unmarked page 1 comment", CreatedAt: "2026-11-27T17:00:00Z"},
		},
		[]mockCommentNode{
			{ID: "IC_p2_1", Author: "wangyu0426", Body: "/ao review-feedback Fix pagination bug", URL: "https://github.com/wangyu0426/WebPoB/pull/9#issuecomment-201", CreatedAt: "2026-11-27T17:10:00Z"},
		},
	)

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	now := time.Date(2026, 11, 27, 17, 15, 0, 0, time.UTC)
	if err := st.UpsertProject(context.Background(), domain.ProjectRecord{
		ID:            "webpob",
		Path:          t.TempDir(),
		RepoOriginURL: "https://github.com/wangyu0426/WebPoB.git",
		RegisteredAt:  now,
		Config: domain.ProjectConfig{
			PRCommentFeedback: domain.PRCommentFeedbackConfig{
				Enabled:        true,
				TrustedAuthors: []string{"wangyu0426"},
			},
		},
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	rec, err := st.CreateSession(context.Background(), domain.SessionRecord{
		ProjectID:        "webpob",
		Kind:             domain.KindWorker,
		Metadata:         domain.SessionMetadata{Branch: "feat/webpob-9"},
		Activity:         domain.Activity{State: domain.ActivityWaitingInput, LastActivityAt: now},
		AutoInjectReview: true,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	prURL := "https://github.com/wangyu0426/WebPoB/pull/9"
	if _, err := st.ClaimPR(context.Background(), domain.PullRequest{
		URL:       prURL,
		SessionID: rec.ID,
		Number:    9,
		HeadSHA:   "deadbeef9999",
		Author:    "wangyu0426",
		Provider:  "github",
		Host:      "github.com",
		Repo:      "wangyu0426/WebPoB",
		UpdatedAt: now,
	}, nil, nil, nil, nil, ports.ReviewWriteReplace, true); err != nil {
		t.Fatalf("ClaimPR: %v", err)
	}

	scmProv, err := scmgithub.NewProvider(scmgithub.ProviderOptions{
		RESTBase:   mockSrv.server.URL,
		GraphQLURL: mockSrv.server.URL + "/graphql",
		Token:      scmgithub.StaticTokenSource("ghp_legacy_test_token"),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("scmgithub.NewProvider: %v", err)
	}
	messenger := &fakeFeedbackMessenger{}
	lcm := lifecycle.New(st, messenger, lifecycle.WithClock(func() time.Time { return now }))
	multi := scmmulti.New(scmmulti.NamedProvider{Key: "github", Provider: scmProv})
	obs := scmobserve.New(multi, st, lcm, scmobserve.Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:  func() time.Time { return now },
	})

	// 1. Worker is input-gated (ActivityWaitingInput): feedback is persisted but deferred.
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatalf("poll while waiting_input: %v", err)
	}
	if len(messenger.snapshot()) != 0 {
		t.Fatalf("expected 0 deliveries while worker is waiting_input, got %d", len(messenger.snapshot()))
	}

	// 2. Worker resumes (ActivityIdle), but messenger fails 4 times (> old 3-attempt cap).
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}
	if err := st.UpdateSession(context.Background(), rec); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	messenger.err = errors.New("temporary delivery failure")
	delays := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}
	for _, d := range delays {
		_ = obs.Poll(context.Background())
		now = now.Add(d)
	}
	// 5th attempt succeeds after restart!
	messenger.err = nil
	restartedLcm := lifecycle.New(st, messenger, lifecycle.WithClock(func() time.Time { return now }))
	restartedObs := scmobserve.New(multi, st, restartedLcm, scmobserve.Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:  func() time.Time { return now },
	})
	if err := restartedObs.Poll(context.Background()); err != nil {
		t.Fatalf("5th attempt after restart: %v", err)
	}
	if msgs := messenger.snapshot(); len(msgs) != 1 || !strings.Contains(msgs[0].message, "Fix pagination bug") {
		t.Fatalf("expected 1 delivered message from page 2 after backoff recovery, got %+v", msgs)
	}

	// 3. Edit policy: editing the comment body delivers the updated feedback.
	now = now.Add(time.Minute)
	mockSrv.setComments([]mockCommentNode{
		{ID: "IC_p2_1", Author: "wangyu0426", Body: "/ao review-feedback Fix pagination bug (updated)", CreatedAt: "2026-11-27T17:10:00Z", UpdatedAt: "2026-11-27T17:20:00Z"},
	})
	if err := restartedObs.Poll(context.Background()); err != nil {
		t.Fatalf("poll after edit: %v", err)
	}
	if msgs := messenger.snapshot(); len(msgs) != 2 || !strings.Contains(msgs[1].message, "(updated)") {
		t.Fatalf("expected updated message after edit, got %+v", msgs)
	}

	// 4. Supersession: posting a newer command supersedes the older one.
	now = now.Add(time.Minute)
	mockSrv.setComments([]mockCommentNode{
		{ID: "IC_p2_1", Author: "wangyu0426", Body: "/ao review-feedback Fix pagination bug (updated)", CreatedAt: "2026-11-27T17:10:00Z"},
		{ID: "IC_p2_2", Author: "wangyu0426", Body: "/ao review-feedback Actually just revert commit 2", CreatedAt: "2026-11-27T17:25:00Z"},
	})
	if err := restartedObs.Poll(context.Background()); err != nil {
		t.Fatalf("poll after supersession: %v", err)
	}
	if msgs := messenger.snapshot(); len(msgs) != 3 || !strings.Contains(msgs[2].message, "Actually just revert commit 2") {
		t.Fatalf("expected superseding message, got %+v", msgs)
	}
	allRows, err := st.ListPRConversationFeedback(context.Background(), prURL)
	if err != nil {
		t.Fatalf("ListPRConversationFeedback: %v", err)
	}
	var sawSuperseded bool
	for _, r := range allRows {
		if r.CommentID == "IC_p2_1" && r.State == domain.PRConversationFeedbackSuperseded {
			sawSuperseded = true
		}
	}
	if !sawSuperseded {
		t.Fatalf("expected IC_p2_1 to be marked superseded, got %+v", allRows)
	}

	// 5. Deletion: removing IC_p2_2 marks it deleted.
	now = now.Add(time.Minute)
	mockSrv.setComments([]mockCommentNode{})
	if err := restartedObs.Poll(context.Background()); err != nil {
		t.Fatalf("poll after deletion: %v", err)
	}
	activeRows, err := st.ListActivePRConversationFeedback(context.Background(), prURL)
	if err != nil {
		t.Fatalf("ListActivePRConversationFeedback: %v", err)
	}
	if len(activeRows) != 0 {
		t.Fatalf("expected 0 active rows after deletion, got %+v", activeRows)
	}
}

func TestPRCommentFeedback_Disabled_HistoricBaseline_And_TerminalPR(t *testing.T) {
	mockSrv := newMockPRServer(t, "wangyu0426", "deadbeef9999")
	mockSrv.setComments([]mockCommentNode{
		{ID: "IC_historic", Author: "wangyu0426", Body: "/ao review-feedback Old historic comment", CreatedAt: "2026-11-27T10:00:00Z"},
	})

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	now := time.Date(2026, 11, 27, 17, 0, 0, 0, time.UTC)
	enabledAt := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	if err := st.UpsertProject(context.Background(), domain.ProjectRecord{
		ID:            "webpob",
		Path:          t.TempDir(),
		RepoOriginURL: "https://github.com/wangyu0426/WebPoB.git",
		RegisteredAt:  now,
		Config: domain.ProjectConfig{
			PRCommentFeedback: domain.PRCommentFeedbackConfig{
				Enabled:        true,
				TrustedAuthors: []string{"wangyu0426"},
				EnabledAt:      &enabledAt,
			},
		},
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	rec, err := st.CreateSession(context.Background(), domain.SessionRecord{
		ProjectID:        "webpob",
		Kind:             domain.KindWorker,
		Metadata:         domain.SessionMetadata{Branch: "feat/webpob-9"},
		Activity:         domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		AutoInjectReview: true,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	prURL := "https://github.com/wangyu0426/WebPoB/pull/9"
	if _, err := st.ClaimPR(context.Background(), domain.PullRequest{
		URL:       prURL,
		SessionID: rec.ID,
		Number:    9,
		HeadSHA:   "deadbeef9999",
		Author:    "wangyu0426",
		Provider:  "github",
		Host:      "github.com",
		Repo:      "wangyu0426/WebPoB",
		UpdatedAt: now,
	}, nil, nil, nil, nil, ports.ReviewWriteReplace, true); err != nil {
		t.Fatalf("ClaimPR: %v", err)
	}

	scmProv, err := scmgithub.NewProvider(scmgithub.ProviderOptions{
		RESTBase:   mockSrv.server.URL,
		GraphQLURL: mockSrv.server.URL + "/graphql",
		Token:      scmgithub.StaticTokenSource("ghp_legacy_test_token"),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("scmgithub.NewProvider: %v", err)
	}
	messenger := &fakeFeedbackMessenger{}
	lcm := lifecycle.New(st, messenger, lifecycle.WithClock(func() time.Time { return now }))
	multi := scmmulti.New(scmmulti.NamedProvider{Key: "github", Provider: scmProv})
	obs := scmobserve.New(multi, st, lcm, scmobserve.Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:  func() time.Time { return now },
	})

	// Historic comment created before EnabledAt must be recorded as baseline and NOT delivered.
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatalf("poll with historic comment: %v", err)
	}
	if len(messenger.snapshot()) != 0 {
		t.Fatalf("historic comment must not be replayed, got %+v", messenger.snapshot())
	}

	// Closed/merged PR must not deliver new commands.
	mockSrv.setPRState("MERGED", true, false)
	mockSrv.setComments([]mockCommentNode{
		{ID: "IC_after_merge", Author: "wangyu0426", Body: "/ao review-feedback After merge", CreatedAt: "2026-11-27T17:05:00Z"},
	})
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatalf("poll on merged PR: %v", err)
	}
	if len(messenger.snapshot()) != 0 {
		t.Fatalf("merged PR must not deliver feedback, got %+v", messenger.snapshot())
	}
}

func TestPRCommentFeedback_DisableRollbackSuppressesDeferredDelivery(t *testing.T) {
	mockSrv := newMockPRServer(t, "wangyu0426", "deadbeef9999")
	mockSrv.setComments([]mockCommentNode{
		{ID: "IC_deferred", Author: "wangyu0426", Body: "/ao review-feedback Deferred command", CreatedAt: "2026-11-27T17:10:00Z"},
	})

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	now := time.Date(2026, 11, 27, 17, 15, 0, 0, time.UTC)
	proj := domain.ProjectRecord{
		ID:            "webpob",
		Path:          t.TempDir(),
		RepoOriginURL: "https://github.com/wangyu0426/WebPoB.git",
		RegisteredAt:  now,
		Config: domain.ProjectConfig{
			PRCommentFeedback: domain.PRCommentFeedbackConfig{
				Enabled:        true,
				TrustedAuthors: []string{"wangyu0426"},
			},
		},
	}
	if err := st.UpsertProject(context.Background(), proj); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	rec, err := st.CreateSession(context.Background(), domain.SessionRecord{
		ProjectID:        "webpob",
		Kind:             domain.KindWorker,
		Metadata:         domain.SessionMetadata{Branch: "feat/webpob-9"},
		Activity:         domain.Activity{State: domain.ActivityWaitingInput, LastActivityAt: now},
		AutoInjectReview: true,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	prURL := "https://github.com/wangyu0426/WebPoB/pull/9"
	if _, err := st.ClaimPR(context.Background(), domain.PullRequest{
		URL:       prURL,
		SessionID: rec.ID,
		Number:    9,
		HeadSHA:   "deadbeef9999",
		Author:    "wangyu0426",
		Provider:  "github",
		Host:      "github.com",
		Repo:      "wangyu0426/WebPoB",
		UpdatedAt: now,
	}, nil, nil, nil, nil, ports.ReviewWriteReplace, true); err != nil {
		t.Fatalf("ClaimPR: %v", err)
	}

	scmProv, err := scmgithub.NewProvider(scmgithub.ProviderOptions{
		RESTBase:   mockSrv.server.URL,
		GraphQLURL: mockSrv.server.URL + "/graphql",
		Token:      scmgithub.StaticTokenSource("ghp_legacy_test_token"),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("scmgithub.NewProvider: %v", err)
	}
	messenger := &fakeFeedbackMessenger{}
	lcm := lifecycle.New(st, messenger, lifecycle.WithClock(func() time.Time { return now }))
	multi := scmmulti.New(scmmulti.NamedProvider{Key: "github", Provider: scmProv})
	obs := scmobserve.New(multi, st, lcm, scmobserve.Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:  func() time.Time { return now },
	})

	// Persist active command while worker is waiting_input (deferred).
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatalf("initial poll: %v", err)
	}
	if len(messenger.snapshot()) != 0 {
		t.Fatalf("expected 0 deliveries while waiting_input, got %d", len(messenger.snapshot()))
	}

	// Operator rolls back by disabling prCommentFeedback.enabled, then worker resumes.
	proj.Config.PRCommentFeedback.Enabled = false
	if err := st.UpsertProject(context.Background(), proj); err != nil {
		t.Fatalf("UpsertProject disable: %v", err)
	}
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}
	if err := st.UpdateSession(context.Background(), rec); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}

	// Trigger a direct PR observation in lifecycle: must NOT deliver the previously deferred row.
	if err := lcm.ApplyPRObservation(context.Background(), rec.ID, ports.PRObservation{
		Fetched: true,
		URL:     prURL,
		Number:  9,
	}); err != nil {
		t.Fatalf("ApplyPRObservation after disable: %v", err)
	}
	if len(messenger.snapshot()) != 0 {
		t.Fatalf("disabled prCommentFeedback must not deliver deferred feedback, got %+v", messenger.snapshot())
	}
}
