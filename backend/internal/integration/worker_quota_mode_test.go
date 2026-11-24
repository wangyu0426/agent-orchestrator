package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/trackerintake"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

type quotaScriptedSource struct {
	mu      sync.Mutex
	now     time.Time
	blocked map[domain.AgentHarness]bool
}

func (s *quotaScriptedSource) Read(_ context.Context, targets []workerquota.Target) (map[workerquota.Target][]workerquota.Window, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[workerquota.Target][]workerquota.Window, len(targets))
	for _, target := range targets {
		out[target] = []workerquota.Window{{
			ObservedAt: s.now,
			ValidUntil: s.now.Add(time.Hour),
			Exhausted:  s.blocked[target.Harness],
		}}
	}
	return out, nil
}

type quotaScriptedChat struct {
	mu              sync.Mutex
	preflightErrs   map[domain.AgentHarness]error
	preflightSeq    map[domain.AgentHarness][]error
	startErrs       map[domain.AgentHarness]error
	backgroundErrs  map[domain.AgentHarness]error
	preflightCalls  []domain.AgentHarness
	startCalls      []sessionmanager.ChatStart
	backgroundCalls []domain.AgentHarness
	stopped         []domain.SessionID
}

func (c *quotaScriptedChat) SupportsChat(domain.AgentHarness) bool { return true }

func (c *quotaScriptedChat) PreflightChat(_ context.Context, harness domain.AgentHarness, _ ports.PermissionMode) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.preflightCalls = append(c.preflightCalls, harness)
	if seq := c.preflightSeq[harness]; len(seq) > 0 {
		err := seq[0]
		if len(seq) > 1 {
			c.preflightSeq[harness] = seq[1:]
		}
		return err
	}
	return c.preflightErrs[harness]
}

func (c *quotaScriptedChat) StartChat(_ context.Context, cfg sessionmanager.ChatStart) (sessionmanager.ChatStarted, error) {
	c.mu.Lock()
	c.startCalls = append(c.startCalls, cfg)
	err := c.startErrs[cfg.Harness]
	c.mu.Unlock()
	if err != nil {
		return sessionmanager.ChatStarted{}, err
	}
	if cfg.ControllerReady != nil {
		if _, commitErr := cfg.ControllerReady(sessionmanager.ChatStarted{
			ProviderConversationID: "conv-" + string(cfg.SessionID),
			ControllerGeneration:   "gen-1",
		}); commitErr != nil {
			return sessionmanager.ChatStarted{}, commitErr
		}
	}
	return sessionmanager.ChatStarted{
		ProviderConversationID: "conv-" + string(cfg.SessionID),
		ControllerGeneration:   "gen-1",
	}, nil
}

func (*quotaScriptedChat) StartChatTurn(context.Context, domain.SessionID, string) (string, error) {
	return "turn-1", nil
}
func (*quotaScriptedChat) RelaySessionChatTurn(context.Context, domain.SessionID, string, string, ports.MessageDeliveryOptions) (string, error) {
	return "turn-1", nil
}
func (*quotaScriptedChat) HasLiveChatController(domain.SessionID) bool { return true }
func (c *quotaScriptedChat) StopChat(_ context.Context, id domain.SessionID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = append(c.stopped, id)
	return nil
}
func (*quotaScriptedChat) QueueChatPrompt(context.Context, domain.SessionID, string) (string, error) {
	return "turn-1", nil
}
func (*quotaScriptedChat) DrainChatQueue(context.Context, domain.SessionID) error { return nil }
func (c *quotaScriptedChat) RunBackgroundTask(_ context.Context, harness domain.AgentHarness, _ ports.ChatStartConfig, _ string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.backgroundCalls = append(c.backgroundCalls, harness)
	if err := c.backgroundErrs[harness]; err != nil {
		return "", err
	}
	return `{"title":"Fix issue","slug":"fix-issue"}`, nil
}

type quotaReadinessSpy struct {
	mu                      sync.Mutex
	snapshots               map[domain.AgentHarness]domain.AgentReadinessSnapshot
	authInvalidated         []string
	installationInvalidated []string
	rechecks                []string
}

func (r *quotaReadinessSpy) EnsureAgentReadiness(_ context.Context, agentID string, _ domain.AgentReadinessPurpose) (domain.AgentReadinessSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if snap, ok := r.snapshots[domain.AgentHarness(agentID)]; ok {
		return snap, nil
	}
	return domain.AgentReadinessSnapshot{
		ID:           agentID,
		Installation: domain.AgentInstallationObservation{State: domain.AgentInstallationInstalled},
	}, nil
}

func (r *quotaReadinessSpy) InvalidateAgentInstallation(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.installationInvalidated = append(r.installationInvalidated, agentID)
}

func (r *quotaReadinessSpy) InvalidateAgentAuthentication(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authInvalidated = append(r.authInvalidated, agentID)
}

func (r *quotaReadinessSpy) RecheckAgent(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rechecks = append(r.rechecks, agentID)
}

type quotaTelemetrySpy struct {
	mu     sync.Mutex
	events []ports.TelemetryEvent
}

func (s *quotaTelemetrySpy) Emit(_ context.Context, ev ports.TelemetryEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}
func (*quotaTelemetrySpy) Close(context.Context) error { return nil }

type quotaTrackingWorkspace struct {
	stubWorkspace
	mu      sync.Mutex
	created int
}

func (w *quotaTrackingWorkspace) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	w.mu.Lock()
	w.created++
	w.mu.Unlock()
	return w.stubWorkspace.Create(ctx, cfg)
}

type quotaModeDefaults domain.SessionMode

func (d quotaModeDefaults) DefaultSessionMode(context.Context) domain.SessionMode {
	return domain.SessionMode(d)
}

type quotaModelCatalog struct{}

func (quotaModelCatalog) Models(_ context.Context, agentID, _ string, _ bool) (ports.AgentModelCatalog, error) {
	if agentID == string(domain.HarnessClaudeCode) {
		return ports.AgentModelCatalog{
			AgentID: agentID,
			Models:  []ports.AgentModelInfo{{ID: "sonnet", IsDefault: true}},
		}, nil
	}
	return ports.AgentModelCatalog{AgentID: agentID}, nil
}

type quotaIssueTracker struct {
	issues []domain.Issue
}

func (t *quotaIssueTracker) Get(context.Context, domain.TrackerID) (domain.Issue, error) {
	return domain.Issue{}, nil
}
func (t *quotaIssueTracker) List(context.Context, domain.TrackerRepo, domain.ListFilter) ([]domain.Issue, error) {
	return t.issues, nil
}
func (*quotaIssueTracker) Preflight(context.Context) error { return nil }

type quotaTestStack struct {
	store     *sqlite.Store
	svc       *sessionsvc.Service
	mgr       *sessionmanager.Manager
	rt        *stubRuntime
	ws        *quotaTrackingWorkspace
	chat      *quotaScriptedChat
	readiness *quotaReadinessSpy
	telemetry *quotaTelemetrySpy
	source    *quotaScriptedSource
	now       time.Time
}

func newQuotaTestStack(t *testing.T, policy *workerquota.Policy, chat *quotaScriptedChat, readiness *quotaReadinessSpy) *quotaTestStack {
	t.Helper()
	ctx := t.Context()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	project := domain.ProjectRecord{
		ID:            "ao",
		Path:          "/repo/ao",
		RepoOriginURL: "https://github.com/acme/ao.git",
		RegisteredAt:  now,
		Config: domain.ProjectConfig{
			Worker: domain.RoleOverride{Harness: domain.HarnessCodex},
			TrackerIntake: domain.TrackerIntakeConfig{
				Enabled:  true,
				Assignee: "alice",
			},
		},
	}
	if err := store.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	msg := &captureMessenger{}
	lcm := lifecycle.New(store, msg)
	rt := &stubRuntime{}
	ws := &quotaTrackingWorkspace{}
	s := &quotaTestStack{
		store:     store,
		rt:        rt,
		ws:        ws,
		chat:      chat,
		readiness: readiness,
		telemetry: &quotaTelemetrySpy{},
		now:       now,
	}
	if source, ok := policy.Source.(*quotaScriptedSource); ok {
		s.source = source
	}
	mgr := sessionmanager.New(sessionmanager.Deps{
		WorkerQuota: policy,
		Runtime:     rt,
		Agents:      stubAgents{},
		Workspace:   ws,
		Store:       store,
		Messenger:   msg,
		Chat:        chat,
		Defaults:    quotaModeDefaults(domain.SessionModeChat),
		Lifecycle:   lcm,
		DataDir:     t.TempDir(),
		Clock:       func() time.Time { return s.now },
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		LookPath:    func(string) (string, error) { return "/usr/bin/true", nil },
	})
	mgr.SetModelCatalog(quotaModelCatalog{})
	mgr.SetTerminalInputGate(integrationTerminalInputGate{})
	mgr.SetAgentReadiness(readiness)
	lcm.SetCompletionTerminator(mgr)
	svc := sessionsvc.NewWithDeps(sessionsvc.Deps{
		Manager:        mgr,
		Store:          store,
		Clock:          func() time.Time { return s.now },
		Telemetry:      s.telemetry,
		AgentReadiness: readiness,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	s.mgr = mgr
	s.svc = svc
	return s
}

func TestWorkerQuotaAGYRouteModeTUILaunchesTUIWithoutACPAuth(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	source := &quotaScriptedSource{now: now, blocked: map[domain.AgentHarness]bool{domain.HarnessCodex: true}}
	chat := &quotaScriptedChat{
		preflightErrs:  map[domain.AgentHarness]error{domain.HarnessAgy: ports.ErrChatAuthRequired},
		startErrs:      map[domain.AgentHarness]error{domain.HarnessAgy: ports.ErrChatAuthRequired},
		backgroundErrs: map[domain.AgentHarness]error{domain.HarnessAgy: ports.ErrChatAuthRequired},
	}
	readiness := &quotaReadinessSpy{snapshots: map[domain.AgentHarness]domain.AgentReadinessSnapshot{
		domain.HarnessAgy: {
			ID:             string(domain.HarnessAgy),
			Installation:   domain.AgentInstallationObservation{State: domain.AgentInstallationInstalled},
			Authentication: domain.AgentAuthenticationObservation{State: domain.AgentAuthenticationUnauthorized, Freshness: domain.AgentReadinessFresh},
		},
	}}
	policy := &workerquota.Policy{
		Fallbacks: []workerquota.Target{{
			Harness: domain.HarnessAgy,
			Model:   "gemini-4-hydrogen-low",
			Mode:    domain.SessionModeTUI,
		}},
		Source: source,
	}
	stack := newQuotaTestStack(t, policy, chat, readiness)

	tracker := &quotaIssueTracker{issues: []domain.Issue{{
		ID:        domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/ao#25"},
		Title:     "Fix routing",
		State:     domain.IssueOpen,
		Assignees: []string{"alice"},
	}}}
	observer := trackerintake.New(
		trackerintake.SingleTrackerResolver{Provider: domain.TrackerProviderGitHub, Adapter: tracker},
		stack.store,
		stack.svc,
		trackerintake.Config{Clock: func() time.Time { return stack.now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
	)
	if err := observer.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := stack.mgr.WaitBackgroundWorkers(t.Context()); err != nil {
		t.Fatal(err)
	}

	sessions, err := stack.store.ListAllSessions(t.Context())
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions = %+v, err=%v; want 1 session", sessions, err)
	}
	got := sessions[0]
	if got.Harness != domain.HarnessAgy || got.Mode != domain.SessionModeTUI || got.Metadata.Model != "gemini-4-hydrogen-low" {
		t.Fatalf("persisted session = %+v (metadata=%+v), want agy/tui/gemini-4-hydrogen-low", got, got.Metadata)
	}
	if stack.rt.created != 1 || len(chat.startCalls) != 0 {
		t.Fatalf("runtime created=%d chat starts=%d; want TUI launch only", stack.rt.created, len(chat.startCalls))
	}
	if len(stack.telemetry.events) == 0 {
		t.Fatal("expected spawned telemetry event")
	}
	spawned := stack.telemetry.events[0]
	if spawned.Name != "ao.sessions.spawned" || spawned.Payload["harness"] != "agy" || spawned.Payload["mode"] != "terminal" {
		t.Fatalf("spawned telemetry = %+v", spawned)
	}
	project, _, _ := stack.store.GetProject(t.Context(), "ao")
	if pauses := stack.svc.AutoSpawnPauses(t.Context(), project); len(pauses) != 0 {
		t.Fatalf("best-effort task naming failure caused pause: %+v", pauses)
	}
}

func TestWorkerQuotaAGYChatAuthFailureAttributesAGYAndPreservesHealthyRoutes(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	source := &quotaScriptedSource{now: now, blocked: map[domain.AgentHarness]bool{domain.HarnessCodex: true}}
	chat := &quotaScriptedChat{
		// ACP handshake fails at session/new with Authentication required, matching incident #26.
		startErrs: map[domain.AgentHarness]error{domain.HarnessAgy: ports.ErrChatAuthRequired},
	}
	readiness := &quotaReadinessSpy{}
	policy := &workerquota.Policy{
		Fallbacks: []workerquota.Target{
			{Harness: domain.HarnessAgy, Model: "gemini-4-hydrogen-low", Mode: domain.SessionModeChat},
			{Harness: domain.HarnessClaudeCode, Model: "sonnet", Mode: domain.SessionModeChat},
		},
		Source: source,
	}
	stack := newQuotaTestStack(t, policy, chat, readiness)

	// Start with only AGY Chat (no second fallback) to verify 3 failures pause AGY/Chat, not Codex.
	policy.Fallbacks = policy.Fallbacks[:1]
	intakeCfg := ports.SpawnConfig{
		ProjectID: "ao",
		IssueID:   "github:acme/ao#25",
		Kind:      domain.KindWorker,
		Prompt:    "Implement feature",
		CreatedBy: domain.SessionCreatedByAutomation,
	}
	for i := range 3 {
		_, _, _, err := stack.svc.Spawn(t.Context(), intakeCfg)
		var apiErr *apierr.Error
		if !errors.As(err, &apiErr) || apiErr.Code != "CHAT_AUTH_REQUIRED" {
			t.Fatalf("attempt %d err = %v, want CHAT_AUTH_REQUIRED", i+1, err)
		}
		if apiErr.Details["harness"] != "agy" || apiErr.Details["mode"] != "chat" {
			t.Fatalf("attempt %d details = %+v, want harness=agy mode=chat", i+1, apiErr.Details)
		}
	}

	if slices.Contains(readiness.authInvalidated, "codex") || !slices.Contains(readiness.authInvalidated, "agy") {
		t.Fatalf("auth invalidated = %v, want agy and never codex", readiness.authInvalidated)
	}
	for _, ev := range stack.telemetry.events {
		if ev.Name == "ao.sessions.spawn_failed" && (ev.Payload["harness"] != "agy" || ev.Payload["mode"] != "chat") {
			t.Fatalf("spawn_failed telemetry misattributed: %+v", ev.Payload)
		}
	}
	if stack.ws.created != stack.ws.destroyed {
		t.Fatalf("leaked workspace on failed admission: created=%d destroyed=%d", stack.ws.created, stack.ws.destroyed)
	}
	if sessions, err := stack.store.ListAllSessions(t.Context()); err != nil {
		t.Fatal(err)
	} else {
		for _, sess := range sessions {
			if !sess.IsTerminated {
				t.Fatalf("leaked active session after failed admission: %+v", sess)
			}
		}
	}

	project, _, _ := stack.store.GetProject(t.Context(), "ao")
	pauses := stack.svc.AutoSpawnPauses(t.Context(), project)
	if len(pauses) != 1 || pauses[0].Harness != domain.HarnessAgy || pauses[0].Mode != domain.SessionModeChat ||
		pauses[0].Model != "gemini-4-hydrogen-low" || pauses[0].ErrorCode != "CHAT_AUTH_REQUIRED" {
		t.Fatalf("pauses = %+v, want single agy/chat CHAT_AUTH_REQUIRED pause", pauses)
	}

	// Fourth attempt with no alternative route returns AUTO_SPAWN_PAUSED attributed to AGY/Chat without launching.
	beforeStarts := len(chat.startCalls)
	_, _, _, err := stack.svc.Spawn(t.Context(), intakeCfg)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "AUTO_SPAWN_PAUSED" ||
		apiErr.Details["harness"] != "agy" || apiErr.Details["mode"] != "chat" || len(chat.startCalls) != beforeStarts {
		t.Fatalf("paused fourth attempt = %v (details=%+v, starts=%d->%d)", err, apiErr, beforeStarts, len(chat.startCalls))
	}

	// Unrelated project is not poisoned by project "ao"'s AGY/Chat pause.
	if err := stack.store.UpsertProject(t.Context(), domain.ProjectRecord{
		ID:           "other",
		Path:         "/repo/other",
		RegisteredAt: stack.now,
		Config:       domain.ProjectConfig{Worker: domain.RoleOverride{Harness: domain.HarnessCodex}},
	}); err != nil {
		t.Fatal(err)
	}
	otherCfg := intakeCfg
	otherCfg.ProjectID = "other"
	otherCfg.IssueID = "github:acme/other#1"
	chat.mu.Lock()
	chat.startErrs[domain.HarnessAgy] = nil
	chat.mu.Unlock()
	if sess, _, _, err := stack.svc.Spawn(t.Context(), otherCfg); err != nil || sess.Harness != domain.HarnessAgy {
		t.Fatalf("unrelated project poisoned by ao pause: sess=%+v err=%v", sess, err)
	}

	// Adding a healthy alternative route (claude-code Chat) remains usable while AGY/Chat is paused on project "ao".
	policy.Fallbacks = []workerquota.Target{
		{Harness: domain.HarnessAgy, Model: "gemini-4-hydrogen-low", Mode: domain.SessionModeChat},
		{Harness: domain.HarnessClaudeCode, Model: "sonnet", Mode: domain.SessionModeChat},
	}
	sess, _, _, err := stack.svc.Spawn(t.Context(), intakeCfg)
	if err != nil {
		t.Fatalf("healthy alternative fallback failed: %v", err)
	}
	if sess.Harness != domain.HarnessClaudeCode || sess.Mode != domain.SessionModeChat || sess.Metadata.Model != "sonnet" {
		t.Fatalf("alternative fallback session = %+v, want claude-code/chat/sonnet", sess)
	}

	// When Codex primary quota recovers, Codex launches normally even while AGY/Chat remains paused.
	source.mu.Lock()
	source.blocked[domain.HarnessCodex] = false
	source.mu.Unlock()
	intakeCfg.IssueID = "github:acme/ao#26"
	sess, _, _, err = stack.svc.Spawn(t.Context(), intakeCfg)
	if err != nil {
		t.Fatalf("recovered primary codex failed: %v", err)
	}
	if sess.Harness != domain.HarnessCodex {
		t.Fatalf("recovered primary harness = %q, want codex", sess.Harness)
	}
}

func TestWorkerQuotaConfiguredPrimaryModeOverride(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	source := &quotaScriptedSource{now: now, blocked: map[domain.AgentHarness]bool{}}
	chat := &quotaScriptedChat{}
	readiness := &quotaReadinessSpy{}
	policy := &workerquota.Policy{
		Primary:   &workerquota.Target{Harness: domain.HarnessAgy, Model: "gemini-4-hydrogen-low", Mode: domain.SessionModeTUI},
		Fallbacks: []workerquota.Target{{Harness: domain.HarnessClaudeCode, Model: "sonnet", Mode: domain.SessionModeChat}},
		Source:    source,
	}
	stack := newQuotaTestStack(t, policy, chat, readiness)

	sess, _, _, err := stack.svc.Spawn(t.Context(), ports.SpawnConfig{
		ProjectID: "ao",
		IssueID:   "github:acme/ao#30",
		Kind:      domain.KindWorker,
		Prompt:    "Run configured primary in TUI",
		CreatedBy: domain.SessionCreatedByAutomation,
	})
	if err != nil {
		t.Fatalf("configured primary TUI spawn failed: %v", err)
	}
	if sess.Harness != domain.HarnessAgy || sess.Mode != domain.SessionModeTUI || sess.Metadata.Model != "gemini-4-hydrogen-low" {
		t.Fatalf("session = %+v, want agy/tui/gemini-4-hydrogen-low", sess)
	}
	if stack.rt.created != 1 || len(chat.startCalls) != 0 {
		t.Fatalf("runtime created=%d chat starts=%d; want TUI only", stack.rt.created, len(chat.startCalls))
	}
}

func TestWorkerQuotaOmittedModePreflightTransitions(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	t.Run("preflight fails then would succeed launches TUI under TUI breaker and skips paused TUI to next route", func(t *testing.T) {
		source := &quotaScriptedSource{now: now, blocked: map[domain.AgentHarness]bool{domain.HarnessCodex: true}}
		chat := &quotaScriptedChat{
			preflightSeq: map[domain.AgentHarness][]error{
				// First check fails with auth required; any duplicate second check would return nil.
				domain.HarnessAgy: {ports.ErrChatAuthRequired, nil},
			},
		}
		policy := &workerquota.Policy{
			Fallbacks: []workerquota.Target{
				{Harness: domain.HarnessAgy, Model: "gemini-4-hydrogen-low"},
				{Harness: domain.HarnessClaudeCode, Model: "sonnet", Mode: domain.SessionModeChat},
			},
			Source: source,
		}
		stack := newQuotaTestStack(t, policy, chat, &quotaReadinessSpy{})
		stack.rt.created = 0

		// Pre-pause AGY Chat to verify an omitted-mode TUI fallback is not blocked by a paused Chat route.
		chat.mu.Lock()
		chat.startErrs = map[domain.AgentHarness]error{domain.HarnessAgy: ports.ErrChatAuthRequired}
		chat.mu.Unlock()
		// Seed 3 AGY/Chat failures directly using an explicit Chat route before testing omitted mode.
		policy.Fallbacks = []workerquota.Target{{Harness: domain.HarnessAgy, Model: "gemini-4-hydrogen-low", Mode: domain.SessionModeChat}}
		intakeCfg := ports.SpawnConfig{
			ProjectID: "ao", IssueID: "github:acme/ao#40", Kind: domain.KindWorker,
			Prompt: "Pause chat first", CreatedBy: domain.SessionCreatedByAutomation,
		}
		for range 3 {
			if _, _, _, err := stack.svc.Spawn(t.Context(), intakeCfg); err == nil {
				t.Fatal("expected AGY chat auth failure")
			}
		}

		// Now switch fallback back to omitted mode, where preflight #1 fails and preflight #2 would succeed.
		chat.mu.Lock()
		chat.startErrs[domain.HarnessAgy] = nil
		chat.preflightCalls = nil
		chat.startCalls = nil
		chat.preflightSeq[domain.HarnessAgy] = []error{ports.ErrChatAuthRequired, nil}
		chat.mu.Unlock()
		policy.Fallbacks = []workerquota.Target{
			{Harness: domain.HarnessAgy, Model: "gemini-4-hydrogen-low"},
			{Harness: domain.HarnessClaudeCode, Model: "sonnet", Mode: domain.SessionModeChat},
		}

		sess, _, _, err := stack.svc.Spawn(t.Context(), intakeCfg)
		if err != nil {
			t.Fatalf("omitted-mode TUI fallback failed: %v", err)
		}
		if sess.Harness != domain.HarnessAgy || sess.Mode != domain.SessionModeTUI {
			t.Fatalf("session = %+v, want agy/tui (must not start Chat under TUI reservation)", sess)
		}
		if len(chat.preflightCalls) != 1 || len(chat.startCalls) != 0 {
			t.Fatalf("preflight calls=%v chat starts=%d, want single preflight and zero chat starts", chat.preflightCalls, len(chat.startCalls))
		}

		// Conversely, if AGY/TUI is paused and preflight resolves to TUI, admission re-selects the healthy next route.
		policy.Fallbacks = []workerquota.Target{{Harness: domain.HarnessAgy, Model: "gemini-4-hydrogen-low", Mode: domain.SessionModeTUI}}
		// Fail AGY/TUI 3 times via an unsupported model? Actually use binary-missing runtime error on TUI?
		// Wait: AGENT_BINARY_NOT_FOUND pauses AGY/TUI. Use a failing runtime? LookPath isn't enough if stubAgent returns ["launch"].
		// Better: pause AGY/TUI cleanly via explicit TUI with failing model? ValidateSpawnModel rejects before admission? No, admitter runs before validateSpawnModel!
		// Wait: agy accepts any model in modelcatalog, so use an unknown/unsupported effort to trip UNSUPPORTED_EFFORT on AGY/TUI!
		policy.Fallbacks = []workerquota.Target{{Harness: domain.HarnessAgy, Model: "gemini-4-hydrogen-low", Effort: "invalid-effort", Mode: domain.SessionModeTUI}}
		for range 3 {
			if _, _, _, err := stack.svc.Spawn(t.Context(), intakeCfg); err == nil {
				t.Fatal("expected AGY TUI config failure")
			}
		}
		chat.mu.Lock()
		chat.preflightSeq[domain.HarnessAgy] = []error{ports.ErrChatAuthRequired, nil}
		chat.mu.Unlock()
		policy.Fallbacks = []workerquota.Target{
			{Harness: domain.HarnessAgy, Model: "gemini-4-hydrogen-low", Effort: "invalid-effort"},
			{Harness: domain.HarnessClaudeCode, Model: "sonnet", Mode: domain.SessionModeChat},
		}
		intakeCfg.IssueID = "github:acme/ao#41"
		sess, _, _, err = stack.svc.Spawn(t.Context(), intakeCfg)
		if err != nil {
			t.Fatalf("expected re-selection to claude-code when resolved TUI is paused: %v", err)
		}
		if sess.Harness != domain.HarnessClaudeCode || sess.Mode != domain.SessionModeChat {
			t.Fatalf("re-selected session = %+v, want claude-code/chat", sess)
		}
	})

	t.Run("preflight succeeds then would fail stays bound to Chat and charges Chat or skips paused Chat", func(t *testing.T) {
		source := &quotaScriptedSource{now: now, blocked: map[domain.AgentHarness]bool{domain.HarnessCodex: true}}
		chat := &quotaScriptedChat{
			preflightSeq: map[domain.AgentHarness][]error{
				domain.HarnessAgy: {nil, ports.ErrChatAuthRequired},
			},
			startErrs: map[domain.AgentHarness]error{
				domain.HarnessAgy: ports.ErrChatAuthRequired,
			},
		}
		policy := &workerquota.Policy{
			Fallbacks: []workerquota.Target{
				{Harness: domain.HarnessAgy, Model: "gemini-4-hydrogen-low"},
			},
			Source: source,
		}
		stack := newQuotaTestStack(t, policy, chat, &quotaReadinessSpy{})
		intakeCfg := ports.SpawnConfig{
			ProjectID: "ao", IssueID: "github:acme/ao#42", Kind: domain.KindWorker,
			Prompt: "Charge chat auth failures to chat", CreatedBy: domain.SessionCreatedByAutomation,
		}
		for i := range 3 {
			chat.mu.Lock()
			chat.preflightSeq[domain.HarnessAgy] = []error{nil, ports.ErrChatAuthRequired}
			chat.mu.Unlock()
			_, _, _, err := stack.svc.Spawn(t.Context(), intakeCfg)
			var apiErr *apierr.Error
			if !errors.As(err, &apiErr) || apiErr.Code != "CHAT_AUTH_REQUIRED" {
				t.Fatalf("attempt %d err = %v, want CHAT_AUTH_REQUIRED", i+1, err)
			}
			if apiErr.Details["harness"] != "agy" || apiErr.Details["mode"] != "chat" {
				t.Fatalf("attempt %d details = %+v, want agy/chat", i+1, apiErr.Details)
			}
		}
		if stack.rt.created != 0 {
			t.Fatalf("unexpected TUI runtime launches = %d", stack.rt.created)
		}
		project, _, _ := stack.store.GetProject(t.Context(), "ao")
		pauses := stack.svc.AutoSpawnPauses(t.Context(), project)
		if len(pauses) != 1 || pauses[0].Harness != domain.HarnessAgy || pauses[0].Mode != domain.SessionModeChat {
			t.Fatalf("pauses = %+v, want agy/chat pause", pauses)
		}

		// With AGY/Chat paused and preflight #1 succeeding, admission skips AGY/Chat and launches the healthy fallback.
		chat.mu.Lock()
		chat.preflightSeq[domain.HarnessAgy] = []error{nil, ports.ErrChatAuthRequired}
		chat.mu.Unlock()
		policy.Fallbacks = []workerquota.Target{
			{Harness: domain.HarnessAgy, Model: "gemini-4-hydrogen-low"},
			{Harness: domain.HarnessClaudeCode, Model: "sonnet", Mode: domain.SessionModeChat},
		}
		sess, _, _, err := stack.svc.Spawn(t.Context(), intakeCfg)
		if err != nil {
			t.Fatalf("healthy fallback failed when AGY/Chat was paused: %v", err)
		}
		if sess.Harness != domain.HarnessClaudeCode || sess.Mode != domain.SessionModeChat {
			t.Fatalf("session = %+v, want claude-code/chat", sess)
		}
	})
}
