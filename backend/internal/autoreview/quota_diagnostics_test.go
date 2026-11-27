package autoreview

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

func seedQuotaFixtureSession(t *testing.T, store *sqlite.Store, now time.Time) domain.SessionRecord {
	t.Helper()
	ctx := t.Context()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID:   "path-of-reasoning",
		Path: t.TempDir(),
		Config: domain.ProjectConfig{
			Reviewers: []domain.ReviewerConfig{{
				Harness:     domain.ReviewerCodex,
				AgentConfig: domain.AgentConfig{Model: "gpt-6-luna", Effort: "max"},
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID:         "path-of-reasoning",
		Kind:              domain.KindWorker,
		Harness:           domain.HarnessCodex,
		AutoReviewEnabled: true,
		Activity:          domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-5 * time.Minute)},
		CreatedAt:         now,
		UpdatedAt:         now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteSCMObservation(ctx, domain.PullRequest{
		URL:       "https://github.com/example/path-of-reasoning/pull/153",
		Number:    153,
		SessionID: sess.ID,
		HeadSHA:   "e4552b1",
		UpdatedAt: now,
	}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}
	return sess
}

func newSanitizedQuotumServer(t *testing.T, payloadFn func() (int, string)) (*httptest.Server, *workerquota.Quotum) {
	t.Helper()
	const secretCookie = "super-secret-quotum-cookie-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("quotum_session")
		if err != nil || c.Value != secretCookie {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		code, body := payloadFn()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	q, err := workerquota.NewQuotum(srv.URL, "main-board", secretCookie, map[domain.AgentHarness]workerquota.Binding{
		domain.HarnessCodex: {SourceID: "codex-main"},
		domain.HarnessAgy: {
			SourceID: "agy-main",
			ModelGroups: map[string][]string{
				"claude-sonnet-5-5-low": {"3p-weekly", "3p-5h"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, q
}

func sanitizedOverviewJSON(now time.Time, agyStale bool, agy3pWeeklyUsed float64, codexResetAt time.Time) string {
	codexSuccess := now.Add(-60 * time.Second).UnixMilli()
	agySuccess := now.Add(-77 * time.Second).UnixMilli()
	codexResetMS := int64(0)
	if !codexResetAt.IsZero() {
		codexResetMS = codexResetAt.UnixMilli()
	}
	return fmt.Sprintf(`{"sources":[
		{
			"id":"codex-main",
			"provider":"codex",
			"stale":false,
			"error":null,
			"successAt":%d,
			"staleAfterMs":420000,
			"windows":[
				{"id":"session","label":null,"used":0,"resetAt":0},
				{"id":"weekly","label":null,"used":100,"resetAt":%d}
			]
		},
		{
			"id":"agy-main",
			"provider":"antigravity",
			"stale":%t,
			"error":null,
			"successAt":%d,
			"staleAfterMs":420000,
			"windows":[
				{"id":"gemini-weekly","label":"Gemini Weekly","used":40.558612,"resetAt":0},
				{"id":"gemini-5h","label":"Gemini 5h","used":16.847569,"resetAt":0},
				{"id":"3p-weekly","label":"3P Weekly","used":%f,"resetAt":0},
				{"id":"3p-5h","label":"3P 5h","used":62.77048,"resetAt":0}
			]
		}
	]}`, codexSuccess, codexResetMS, agyStale, agySuccess, agy3pWeeklyUsed)
}

func TestReviewerQuotaSanitizedFixtureColdStartAndPersistedDeferralRecovery(t *testing.T) {
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)

	primary := workerquota.Target{
		Harness: domain.HarnessCodex,
		Model:   "gpt-6-luna",
		Effort:  "max",
		Mode:    domain.SessionModeChat,
	}
	fallback := workerquota.Target{
		Harness: domain.HarnessAgy,
		Model:   "claude-sonnet-5-5-low",
		Mode:    domain.SessionModeTUI,
	}

	for _, tc := range []struct {
		name             string
		preexistingDefer bool
	}{
		{name: "cold_start_positive_sonnet_fallback", preexistingDefer: false},
		{name: "persisted_deferral_recovers_on_positive_sonnet_fallback", preexistingDefer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := sqlitetest.MustOpen(t)
			sess := seedQuotaFixtureSession(t, store, now)
			if tc.preexistingDefer {
				if err := store.UpsertReviewerQuotaDeferral(
					ctx, sess.ID, "https://github.com/example/path-of-reasoning/pull/153", "e4552b1",
					now.Add(10*time.Minute), "reviewer_quota_deferred", now.Add(-2*time.Minute),
				); err != nil {
					t.Fatal(err)
				}
			}

			_, quotum := newSanitizedQuotumServer(t, func() (int, string) {
				return http.StatusOK, sanitizedOverviewJSON(now, false, 31.385243, time.Time{})
			})

			var logBuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			trig := &recordingOptionsTrigger{}
			coord := New(store, trig, Config{
				Clock:  func() time.Time { return now },
				Logger: logger,
				Quota: &workerquota.Policy{
					Role:      workerquota.RoleReviewer,
					Primary:   &primary,
					Fallbacks: []workerquota.Target{fallback},
					Source:    quotum,
				},
			})

			res, err := coord.EvaluateSession(ctx, sess.ID)
			if err != nil || !res.Triggered || len(trig.calls) != 1 {
				t.Fatalf("EvaluateSession = %+v, err=%v, calls=%d", res, err, len(trig.calls))
			}
			got := trig.calls[0]
			if got.Harness != domain.ReviewerAgy || got.Config.Model != "claude-sonnet-5-5-low" || got.InterfaceMode != domain.ReviewerInterfaceTUI {
				t.Fatalf("selected trigger options = %+v, want agy/claude-sonnet-5-5-low/tui", got)
			}

			// Verify deferral was cleared.
			if _, _, found, err := store.GetReviewerQuotaDeferral(ctx, sess.ID, "https://github.com/example/path-of-reasoning/pull/153", "e4552b1"); err != nil || found {
				t.Fatalf("expected persisted deferral to be cleared, found=%v err=%v", found, err)
			}

			// Verify structured diagnostics logged route details and never leaked secrets or raw payloads.
			logs := logBuf.String()
			for _, want := range []string{
				"auto-review: reviewer quota route evaluated",
				"harness=codex",
				"evidence=exhausted",
				"decision=quota_exhausted",
				"harness=agy",
				"model=claude-sonnet-5-5-low",
				"mode=tui",
				"matched_windows=2",
				"evidence=available",
				"fresh_positive=true",
				"decision=selected",
			} {
				if !strings.Contains(logs, want) {
					t.Errorf("expected log to contain %q, got:\n%s", want, logs)
				}
			}
			for _, forbidden := range []string{"super-secret-quotum-cookie-value", "quotum_session", `"sources":[`} {
				if strings.Contains(logs, forbidden) {
					t.Errorf("log must not contain %q, got:\n%s", forbidden, logs)
				}
			}

			// Repeated polling after a review is already approved on this HEAD must not duplicate the review.
			if err := store.UpsertReview(ctx, domain.Review{
				ID: "rev-agy-approved", SessionID: sess.ID, ProjectID: "path-of-reasoning",
				Harness: domain.ReviewerAgy, InterfaceMode: domain.ReviewerInterfaceTUI,
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.InsertReviewRun(ctx, domain.ReviewRun{
				ID:            "run-agy-approved",
				ReviewID:      "rev-agy-approved",
				SessionID:     sess.ID,
				PRURL:         "https://github.com/example/path-of-reasoning/pull/153",
				TargetSHA:     "e4552b1",
				Harness:       domain.ReviewerAgy,
				Status:        domain.ReviewRunComplete,
				Verdict:       domain.VerdictApproved,
				TriggerSource: domain.ReviewTriggerAuto,
				CreatedAt:     now,
			}); err != nil {
				t.Fatal(err)
			}
			res2, err := coord.EvaluateSession(ctx, sess.ID)
			if err != nil {
				t.Fatalf("second EvaluateSession err=%v", err)
			}
			if res2.Triggered || len(trig.calls) != 1 || res2.Reason != "already_approved" {
				t.Fatalf("repeated poll should not duplicate review: res=%+v calls=%d", res2, len(trig.calls))
			}
		})
	}
}

func TestReviewerQuotaDiagnosticsDistinguishExhaustionStaleUnreadyAndRetryCap(t *testing.T) {
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	primary := workerquota.Target{Harness: domain.HarnessCodex, Model: "gpt-6-luna", Effort: "max", Mode: domain.SessionModeChat}
	fallback := workerquota.Target{Harness: domain.HarnessAgy, Model: "claude-sonnet-5-5-low", Mode: domain.SessionModeTUI}

	t.Run("genuine_exhausted_fallback_and_snapshot_expiry_vs_quota_reset", func(t *testing.T) {
		store := sqlitetest.MustOpen(t)
		sess := seedQuotaFixtureSession(t, store, now)
		_, quotum := newSanitizedQuotumServer(t, func() (int, string) {
			return http.StatusOK, sanitizedOverviewJSON(now, false, 100, time.Time{})
		})
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		coord := New(store, &recordingOptionsTrigger{}, Config{
			Clock:  func() time.Time { return now },
			Logger: logger,
			Quota: &workerquota.Policy{
				Role:      workerquota.RoleReviewer,
				Primary:   &primary,
				Fallbacks: []workerquota.Target{fallback},
				Source:    quotum,
			},
		})
		res, err := coord.EvaluateSession(ctx, sess.ID)
		if err != nil || res.Triggered || res.Reason != "reviewer_quota_deferred" {
			t.Fatalf("EvaluateSession = %+v, err=%v", res, err)
		}
		// Identical second poll should downgrade deferral log to DEBUG (no INFO poll spam).
		res2, err := coord.EvaluateSession(ctx, sess.ID)
		if err != nil || res2.Triggered || res2.Reason != "reviewer_quota_deferred" {
			t.Fatalf("second EvaluateSession = %+v, err=%v", res2, err)
		}
		logs := logBuf.String()
		if !strings.Contains(logs, `level=INFO msg="auto-review: all reviewer quota routes exhausted; deferring"`) {
			t.Errorf("expected INFO all-exhausted deferral log, got:\n%s", logs)
		}
		if !strings.Contains(logs, "retry_at_source=snapshot_expiry") || !strings.Contains(logs, "all_routes_exhausted=true") {
			t.Errorf("expected snapshot_expiry and all_routes_exhausted=true, got:\n%s", logs)
		}
		if strings.Count(logs, `level=INFO msg="auto-review: all reviewer quota routes exhausted; deferring"`) != 1 {
			t.Errorf("expected transition-based INFO log (1 occurrence), got:\n%s", logs)
		}
		if !strings.Contains(logs, `level=DEBUG msg="auto-review: all reviewer quota routes exhausted; deferring"`) {
			t.Errorf("expected repeated identical poll to log at DEBUG, got:\n%s", logs)
		}
	})

	t.Run("stale_fallback_during_active_deferral_distinguished_from_exhaustion", func(t *testing.T) {
		store := sqlitetest.MustOpen(t)
		sess := seedQuotaFixtureSession(t, store, now)
		if err := store.UpsertReviewerQuotaDeferral(
			ctx, sess.ID, "https://github.com/example/path-of-reasoning/pull/153", "e4552b1",
			now.Add(10*time.Minute), "reviewer_quota_deferred", now.Add(-2*time.Minute),
		); err != nil {
			t.Fatal(err)
		}
		_, quotum := newSanitizedQuotumServer(t, func() (int, string) {
			return http.StatusOK, sanitizedOverviewJSON(now, true, 31.385243, now.Add(2*time.Minute))
		})
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		coord := New(store, &recordingOptionsTrigger{}, Config{
			Clock:  func() time.Time { return now },
			Logger: logger,
			Quota: &workerquota.Policy{
				Role:      workerquota.RoleReviewer,
				Primary:   &primary,
				Fallbacks: []workerquota.Target{fallback},
				Source:    quotum,
			},
		})
		res, err := coord.EvaluateSession(ctx, sess.ID)
		if err != nil || res.Triggered || res.Reason != "reviewer_quota_deferred" {
			t.Fatalf("EvaluateSession = %+v, err=%v", res, err)
		}
		logs := logBuf.String()
		for _, want := range []string{
			`msg="auto-review: no eligible reviewer quota route available; deferring"`,
			"retry_at_source=quota_reset",
			"all_routes_exhausted=false",
			"exhausted_routes=1",
			"unknown_during_deferral_routes=1",
			"decision=deferral_requires_fresh_positive",
		} {
			if !strings.Contains(logs, want) {
				t.Errorf("expected log to contain %q, got:\n%s", want, logs)
			}
		}
	})

	t.Run("malformed_snapshot_retains_active_deferral", func(t *testing.T) {
		store := sqlitetest.MustOpen(t)
		sess := seedQuotaFixtureSession(t, store, now)
		retryAt := now.Add(10 * time.Minute)
		if err := store.UpsertReviewerQuotaDeferral(
			ctx, sess.ID, "https://github.com/example/path-of-reasoning/pull/153", "e4552b1",
			retryAt, "reviewer_quota_deferred", now.Add(-2*time.Minute),
		); err != nil {
			t.Fatal(err)
		}
		_, quotum := newSanitizedQuotumServer(t, func() (int, string) {
			return http.StatusOK, `{"not_sources": true}`
		})
		coord := New(store, &recordingOptionsTrigger{}, Config{
			Clock: func() time.Time { return now },
			Quota: &workerquota.Policy{
				Role:      workerquota.RoleReviewer,
				Primary:   &primary,
				Fallbacks: []workerquota.Target{fallback},
				Source:    quotum,
			},
		})
		res, err := coord.EvaluateSession(ctx, sess.ID)
		if err != nil || res.Triggered || res.Reason != "reviewer_quota_deferred" || !res.RetryAt.Equal(retryAt) {
			t.Fatalf("malformed snapshot should retain deferral: res=%+v err=%v", res, err)
		}
	})

	t.Run("unready_fallback_and_no_ready_route", func(t *testing.T) {
		store := sqlitetest.MustOpen(t)
		sess := seedQuotaFixtureSession(t, store, now)
		_, quotum := newSanitizedQuotumServer(t, func() (int, string) {
			return http.StatusOK, sanitizedOverviewJSON(now, false, 31.385243, time.Time{})
		})
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		coord := New(store, &recordingOptionsTrigger{}, Config{
			Clock:  func() time.Time { return now },
			Logger: logger,
			Quota: &workerquota.Policy{
				Role:      workerquota.RoleReviewer,
				Primary:   &primary,
				Fallbacks: []workerquota.Target{fallback},
				Source:    quotum,
			},
			RouteReady: func(context.Context, workerquota.Target) bool { return false },
		})
		res, err := coord.EvaluateSession(ctx, sess.ID)
		if err != nil || res.Triggered || res.Reason != "reviewer_quota_deferred" {
			t.Fatalf("EvaluateSession = %+v, err=%v", res, err)
		}
		logs := logBuf.String()
		for _, want := range []string{"decision=route_not_ready", "unready_routes=1", "all_routes_exhausted=false"} {
			if !strings.Contains(logs, want) {
				t.Errorf("expected log to contain %q, got:\n%s", want, logs)
			}
		}

		// When no route is exhausted and all routes are unready, reason is reviewer_quota_no_ready_route.
		store2 := sqlitetest.MustOpen(t)
		sess2 := seedQuotaFixtureSession(t, store2, now)
		coord2 := New(store2, &recordingOptionsTrigger{}, Config{
			Clock: func() time.Time { return now },
			Quota: &workerquota.Policy{
				Role:      workerquota.RoleReviewer,
				Primary:   &primary,
				Fallbacks: []workerquota.Target{fallback},
			},
			RouteReady: func(context.Context, workerquota.Target) bool { return false },
		})
		res2, err := coord2.EvaluateSession(ctx, sess2.ID)
		if err != nil || res2.Triggered || res2.Reason != "reviewer_quota_no_ready_route" {
			t.Fatalf("expected reviewer_quota_no_ready_route, got %+v err=%v", res2, err)
		}
	})

	t.Run("retry_capped_fallback_logged_distinctly", func(t *testing.T) {
		store := sqlitetest.MustOpen(t)
		sess := seedQuotaFixtureSession(t, store, now)
		if err := store.UpsertReview(ctx, domain.Review{
			ID: "rev-agy-cap", SessionID: sess.ID, ProjectID: "path-of-reasoning",
			Harness: domain.ReviewerAgy, InterfaceMode: domain.ReviewerInterfaceTUI,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			if err := store.InsertReviewRun(ctx, domain.ReviewRun{
				ID:            fmt.Sprintf("run-agy-%d", i),
				ReviewID:      "rev-agy-cap",
				SessionID:     sess.ID,
				PRURL:         "https://github.com/example/path-of-reasoning/pull/153",
				TargetSHA:     "e4552b1",
				Harness:       domain.ReviewerAgy,
				Status:        domain.ReviewRunFailed,
				TriggerSource: domain.ReviewTriggerAuto,
				CreatedAt:     now.Add(time.Duration(i) * time.Second),
			}); err != nil {
				t.Fatal(err)
			}
		}
		_, quotum := newSanitizedQuotumServer(t, func() (int, string) {
			return http.StatusOK, sanitizedOverviewJSON(now, false, 31.385243, time.Time{})
		})
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		coord := New(store, &recordingOptionsTrigger{}, Config{
			Clock:  func() time.Time { return now },
			Logger: logger,
			Quota: &workerquota.Policy{
				Role:      workerquota.RoleReviewer,
				Primary:   &primary,
				Fallbacks: []workerquota.Target{fallback},
				Source:    quotum,
			},
		})
		res, err := coord.EvaluateSession(ctx, sess.ID)
		if err != nil || res.Triggered || res.Reason != "reviewer_quota_deferred" {
			t.Fatalf("EvaluateSession = %+v, err=%v", res, err)
		}
		logs := logBuf.String()
		for _, want := range []string{"decision=route_retry_deferred", "retry_capped_routes=1", "all_routes_exhausted=false"} {
			if !strings.Contains(logs, want) {
				t.Errorf("expected log to contain %q, got:\n%s", want, logs)
			}
		}
	})
}
