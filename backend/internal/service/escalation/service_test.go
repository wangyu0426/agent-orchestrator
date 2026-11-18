package escalation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fakeReader struct {
	mu        sync.Mutex
	sessions  map[domain.SessionID]domain.SessionRecord
	convs     map[domain.SessionID]domain.ConversationRecord
	turns     map[string]domain.ConversationTurn
	pending   map[string]bool
	activeTxn map[domain.SessionID]bool
	launch    map[domain.SessionID]domain.AgentLaunchOptions
	// reports are persisted Done (and other) reports, as CreateReport
	// would have stored them, whether or not HandleReportDone ran.
	reports map[domain.SessionID][]domain.ReportRecord
}

func (f *fakeReader) ListReportsBySession(_ context.Context, id domain.SessionID) ([]domain.ReportRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]domain.ReportRecord(nil), f.reports[id]...)
	return out, nil
}

func (f *fakeReader) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.sessions[id]
	return rec, ok, nil
}

func (f *fakeReader) ConversationForSession(_ context.Context, id domain.SessionID) (domain.ConversationRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	conv, ok := f.convs[id]
	if !ok {
		return domain.ConversationRecord{}, fmt.Errorf("conversation not found for %s", id)
	}
	return conv, nil
}

func (f *fakeReader) LatestConversationTurn(_ context.Context, conversationID string) (domain.ConversationTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	turn, ok := f.turns[conversationID]
	if !ok {
		return domain.ConversationTurn{}, domain.ErrNoConversationTurn
	}
	return turn, nil
}

func (f *fakeReader) HasPendingConversationInteractions(_ context.Context, conversationID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending[conversationID], nil
}

func (f *fakeReader) GetActiveSessionInterfaceTransition(_ context.Context, id domain.SessionID) (domain.SessionInterfaceTransition, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.activeTxn[id] {
		return domain.SessionInterfaceTransition{SessionID: id}, true, nil
	}
	return domain.SessionInterfaceTransition{}, false, nil
}

func (f *fakeReader) SessionLaunchOptions(_ context.Context, id domain.SessionID) (domain.AgentLaunchOptions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launch[id], nil
}

func (f *fakeReader) setActivity(id domain.SessionID, state domain.ActivityState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.sessions[id]
	rec.Activity = domain.Activity{State: state, LastActivityAt: time.Now()}
	f.sessions[id] = rec
}

func (f *fakeReader) setGeneration(id domain.SessionID, gen string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.sessions[id]
	rec.Metadata.ControllerGeneration = gen
	f.sessions[id] = rec
}

func (f *fakeReader) setTurn(conversationID, turnID string, state domain.TurnState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Turns created after an escalation starts are requested after it:
	// the fixed timestamp is after newTestEnv's Start time (12:00) so
	// BindWorkItem's creation-time check passes for them.
	f.turns[conversationID] = domain.ConversationTurn{
		ID: turnID, ConversationID: conversationID, State: state,
		RequestedAt: time.Date(2026, 11, 18, 12, 30, 0, 0, time.UTC),
	}
}

type fakeStore struct {
	mu  sync.Mutex
	esc map[domain.SessionID]*domain.TemporaryEscalation
}

func (s *fakeStore) copy(e *domain.TemporaryEscalation) *domain.TemporaryEscalation {
	if e == nil {
		return nil
	}
	cp := *e
	return &cp
}

func (s *fakeStore) GetEscalation(_ context.Context, id domain.SessionID) (*domain.TemporaryEscalation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.esc[id]
	return s.copy(e), ok, nil
}

func (s *fakeStore) CreateEscalation(_ context.Context, esc *domain.TemporaryEscalation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.esc[esc.SessionID]; ok && (cur.State.Active() || cur.State == domain.TemporaryEscalationFailed) {
		return fmt.Errorf("escalation already exists for %s", esc.SessionID)
	}
	if s.esc == nil {
		s.esc = map[domain.SessionID]*domain.TemporaryEscalation{}
	}
	s.esc[esc.SessionID] = s.copy(esc)
	return nil
}

func (s *fakeStore) UpdateEscalation(_ context.Context, esc *domain.TemporaryEscalation, from domain.TemporaryEscalationState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.esc[esc.SessionID]
	if !ok || cur.State != from {
		return false, nil
	}
	s.esc[esc.SessionID] = s.copy(esc)
	return true, nil
}

func (s *fakeStore) DeleteEscalation(_ context.Context, id domain.SessionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.esc, id)
	return nil
}

func (s *fakeStore) ListActiveEscalations(_ context.Context) ([]*domain.TemporaryEscalation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*domain.TemporaryEscalation
	for _, e := range s.esc {
		if e.State.Active() || e.State == domain.TemporaryEscalationFailed {
			out = append(out, s.copy(e))
		}
	}
	return out, nil
}

type fakeSwitcher struct {
	mu           sync.Mutex
	reader       *fakeReader
	switches     []domain.ProviderSwitchRequest
	settingsSets []domain.ConversationSettings
	failNext     int
	preflightErr error
	// preserveConvSettings simulates a switch that lands the
	// session-level model but leaves the Chat controller settings
	// untouched (wrong effort/settings after revert).
	preserveConvSettings bool
}

func (s *fakeSwitcher) SwitchProvider(_ context.Context, id domain.SessionID, req domain.ProviderSwitchRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.switches = append(s.switches, req)
	if s.failNext > 0 {
		s.failNext--
		return errors.New("target provider failed to start")
	}
	rec := s.reader.sessions[id]
	if req.TargetHarness == rec.Harness {
		return domain.ErrProviderAlreadyActive
	}
	rec.Harness = req.TargetHarness
	rec.Metadata.Model = req.Model
	rec.Metadata.Effort = req.Effort
	rec.Metadata.ControllerGeneration = "gen-2"
	s.reader.sessions[id] = rec
	if !s.preserveConvSettings {
		// A real cross-harness switch saves the target's settings
		// with the switch, so the conversation reflects the new
		// controller.
		conv := s.reader.convs[id]
		conv.Settings.Model = req.Model
		conv.Settings.ReasoningEffort = req.Effort
		if req.PermissionMode != "" {
			conv.Settings.ApprovalMode = req.PermissionMode
		}
		s.reader.convs[id] = conv
	}
	return nil
}

func (s *fakeSwitcher) SetTurnSettings(_ context.Context, id domain.SessionID, settings domain.ConversationSettings) (domain.ConversationSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settingsSets = append(s.settingsSets, settings)
	rec := s.reader.sessions[id]
	rec.Metadata.Model = settings.Model
	rec.Metadata.Effort = settings.ReasoningEffort
	s.reader.sessions[id] = rec
	if !s.preserveConvSettings {
		conv := s.reader.convs[id]
		conv.Settings.Model = settings.Model
		conv.Settings.ReasoningEffort = settings.ReasoningEffort
		conv.Settings.ApprovalMode = settings.ApprovalMode
		s.reader.convs[id] = conv
	}
	return settings, nil
}

func (s *fakeSwitcher) PreflightChat(_ context.Context, _ domain.AgentHarness, _ ports.PermissionMode) error {
	return s.preflightErr
}

type testEnv struct {
	svc      *Service
	reader   *fakeReader
	store    *fakeStore
	switcher *fakeSwitcher
	now      time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	reader := &fakeReader{
		sessions: map[domain.SessionID]domain.SessionRecord{
			"sess-1": {
				ID: "sess-1", ProjectID: "proj-1", Kind: domain.KindWorker,
				Harness: domain.HarnessCodex, Mode: domain.SessionModeChat,
				Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
				Metadata: domain.SessionMetadata{
					Model: "mai-code-2", Effort: "medium",
					ControllerGeneration: "gen-1", Permissions: domain.PermissionModeAuto,
				},
			},
		},
		convs: map[domain.SessionID]domain.ConversationRecord{
			"sess-1": {
				ID: "conv-1", SessionID: "sess-1",
				Settings: domain.ConversationSettings{
					Model: "mai-code-2", ReasoningEffort: "medium",
					ApprovalMode: domain.PermissionModeAuto,
				},
			},
		},
		turns: map[string]domain.ConversationTurn{
			"conv-1": {ID: "turn-1", ConversationID: "conv-1", State: domain.TurnStateCompleted},
		},
		pending:   map[string]bool{},
		activeTxn: map[domain.SessionID]bool{},
		launch:    map[domain.SessionID]domain.AgentLaunchOptions{"sess-1": {}},
	}
	store := &fakeStore{esc: map[domain.SessionID]*domain.TemporaryEscalation{}}
	switcher := &fakeSwitcher{reader: reader}
	current := now
	svc := New(Deps{
		Reader: reader, Store: store, Switcher: switcher,
		Now:   func() time.Time { return current },
		NewID: func() string { return "esc-1" },
	})
	return &testEnv{svc: svc, reader: reader, store: store, switcher: switcher, now: now}
}

func (e *testEnv) advance(d time.Duration) { e.now = e.now.Add(d) }

func startUpgrade(t *testing.T, env *testEnv) *domain.TemporaryEscalation {
	t.Helper()
	esc, err := env.svc.Start(context.Background(), "sess-1", domain.ProviderSwitchRequest{
		TargetHarness: domain.HarnessClaudeCode, Model: "sol-6.2", Effort: "high",
	})
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	return esc
}

// startUpgradeWithTask starts an escalation and binds it to a new turn,
// the way the temporary switch+message controller path does: the
// escalated task is the turn created after the switch, not the
// completed turn that was latest at Start time.
func startUpgradeWithTask(t *testing.T, env *testEnv, turnID string) *domain.TemporaryEscalation {
	t.Helper()
	esc := startUpgrade(t, env)
	env.reader.setTurn("conv-1", turnID, domain.TurnStateRunning)
	if err := env.svc.BindWorkItem(context.Background(), "sess-1", turnID); err != nil {
		t.Fatalf("BindWorkItem() = %v", err)
	}
	return esc
}

func doneReport(env *testEnv, id string) domain.ReportRecord {
	return domain.ReportRecord{
		ID: id, SessionID: "sess-1", ProjectID: "proj-1",
		State: domain.ReportDone, Note: "done", CreatedAt: env.now,
	}
}

func TestStartCrossHarnessSnapshotsOriginal(t *testing.T) {
	env := newTestEnv(t)
	esc := startUpgrade(t, env)
	if esc.State != domain.TemporaryEscalationPending {
		t.Fatalf("state = %q, want pending", esc.State)
	}
	if esc.Original.Harness != domain.HarnessCodex || esc.Original.Model != "mai-code-2" || esc.Original.Effort != "medium" {
		t.Fatalf("snapshot = %+v, want codex/mai-code-2/medium", esc.Original)
	}
	// Start leaves the work item unbound: the completed turn-1
	// predates the upgrade, and the escalated task is the next turn.
	if esc.WorkItemID != "" || esc.GenerationBefore != "gen-1" || esc.GenerationAfter != "gen-2" {
		t.Fatalf("esc = %+v, want unbound workItem gen-1->gen-2", esc)
	}
	rec, _, _ := env.reader.GetSession(context.Background(), "sess-1")
	if rec.Harness != domain.HarnessClaudeCode || rec.Metadata.Model != "sol-6.2" {
		t.Fatalf("session = %s/%s, want claude-code/sol-6.2", rec.Harness, rec.Metadata.Model)
	}
	if len(env.switcher.switches) != 1 {
		t.Fatalf("switches = %d, want 1", len(env.switcher.switches))
	}
}

func TestStartSameHarnessChatUsesTurnSettings(t *testing.T) {
	env := newTestEnv(t)
	esc, err := env.svc.Start(context.Background(), "sess-1", domain.ProviderSwitchRequest{
		TargetHarness: domain.HarnessCodex, Model: "mai-code-3", Effort: "high",
	})
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if len(env.switcher.switches) != 0 || len(env.switcher.settingsSets) != 1 {
		t.Fatalf("switches = %d settingsSets = %d, want 0/1", len(env.switcher.switches), len(env.switcher.settingsSets))
	}
	if env.switcher.settingsSets[0].Model != "mai-code-3" {
		t.Fatalf("settings = %+v", env.switcher.settingsSets[0])
	}
	if esc.TargetHarness != domain.HarnessCodex {
		t.Fatalf("target = %q", esc.TargetHarness)
	}
}

func TestStartRefusals(t *testing.T) {
	target := domain.ProviderSwitchRequest{TargetHarness: domain.HarnessClaudeCode, Model: "sol-6.2"}
	t.Run("busy worker", func(t *testing.T) {
		env := newTestEnv(t)
		env.reader.setActivity("sess-1", domain.ActivityActive)
		if _, err := env.svc.Start(context.Background(), "sess-1", target); !errors.Is(err, ErrEscalationBusy) {
			t.Errorf("Start() = %v, want ErrEscalationBusy", err)
		}
	})
	t.Run("pending interaction", func(t *testing.T) {
		env := newTestEnv(t)
		env.reader.pending["conv-1"] = true
		if _, err := env.svc.Start(context.Background(), "sess-1", target); !errors.Is(err, ErrPendingInteraction) {
			t.Errorf("Start() = %v, want ErrPendingInteraction", err)
		}
	})
	t.Run("interface transition in flight", func(t *testing.T) {
		env := newTestEnv(t)
		env.reader.activeTxn["sess-1"] = true
		if _, err := env.svc.Start(context.Background(), "sess-1", target); !errors.Is(err, ErrEscalationBusy) {
			t.Errorf("Start() = %v, want ErrEscalationBusy", err)
		}
	})
	t.Run("nested escalation", func(t *testing.T) {
		env := newTestEnv(t)
		startUpgrade(t, env)
		if _, err := env.svc.Start(context.Background(), "sess-1", target); !errors.Is(err, ErrEscalationActive) {
			t.Errorf("second Start() = %v, want ErrEscalationActive", err)
		}
	})
	t.Run("already on target model", func(t *testing.T) {
		env := newTestEnv(t)
		_, err := env.svc.Start(context.Background(), "sess-1", domain.ProviderSwitchRequest{
			TargetHarness: domain.HarnessCodex, Model: "mai-code-2", Effort: "medium",
		})
		if !errors.Is(err, ErrEscalationUnsupported) {
			t.Errorf("Start() = %v, want ErrEscalationUnsupported", err)
		}
	})
	// Same-harness TUI (including Codex exec workers) is unsupported in
	// v1: a TUI model's launch choices only take effect at start and AO
	// has no relaunch-with-new-model path. An Orchestrator call cannot
	// bypass this; the gap is tracked as follow-up #11, not fixed here.
	t.Run("same-harness TUI (Codex exec) is unsupported in v1", func(t *testing.T) {
		env := newTestEnv(t)
		env.reader.sessions["sess-1"] = func() domain.SessionRecord {
			rec, _, _ := env.reader.GetSession(context.Background(), "sess-1")
			rec.Mode = domain.SessionModeTUI
			return rec
		}()
		_, err := env.svc.Start(context.Background(), "sess-1", domain.ProviderSwitchRequest{
			TargetHarness: domain.HarnessCodex, Model: "mai-code-3",
		})
		if !errors.Is(err, ErrEscalationUnsupported) {
			t.Errorf("Start() = %v, want ErrEscalationUnsupported", err)
		}
	})
	t.Run("unknown session", func(t *testing.T) {
		env := newTestEnv(t)
		if _, err := env.svc.Start(context.Background(), "nope", target); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("Start() = %v, want ErrSessionNotFound", err)
		}
	})
}

func TestStartSwitchFailurePreservesWorker(t *testing.T) {
	env := newTestEnv(t)
	env.switcher.failNext = 1
	_, err := env.svc.Start(context.Background(), "sess-1", domain.ProviderSwitchRequest{
		TargetHarness: domain.HarnessClaudeCode, Model: "sol-6.2",
	})
	if err == nil {
		t.Fatal("Start() = nil, want error")
	}
	rec, _, _ := env.reader.GetSession(context.Background(), "sess-1")
	if rec.Harness != domain.HarnessCodex || rec.Metadata.Model != "mai-code-2" {
		t.Fatalf("session moved to %s/%s, want codex/mai-code-2", rec.Harness, rec.Metadata.Model)
	}
	if _, found, _ := env.svc.Get(context.Background(), "sess-1"); found {
		t.Error("escalation still stored after a failed upgrade switch")
	}
}

func TestDoneThenIdleRevertsExactlyOnce(t *testing.T) {
	env := newTestEnv(t)
	startUpgradeWithTask(t, env, "turn-2")
	// The worker is still finishing: done arrives but the turn is running.
	env.reader.setActivity("sess-1", domain.ActivityActive)
	env.reader.setTurn("conv-1", "turn-2", domain.TurnStateRunning)
	env.advance(time.Minute)
	if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-1")); err != nil {
		t.Fatalf("HandleReportDone() = %v", err)
	}
	esc, _, _ := env.svc.Get(context.Background(), "sess-1")
	if esc.State != domain.TemporaryEscalationAwaitingIdle {
		t.Fatalf("state = %q, want awaiting_idle", esc.State)
	}
	rec, _, _ := env.reader.GetSession(context.Background(), "sess-1")
	if rec.Harness != domain.HarnessClaudeCode {
		t.Fatalf("reverted early: harness = %s", rec.Harness)
	}
	// Idle without a new done must not revert on its own... except the
	// done is already recorded, so reaching idle does revert.
	env.reader.setActivity("sess-1", domain.ActivityIdle)
	env.reader.setTurn("conv-1", "turn-2", domain.TurnStateCompleted)
	reverted, err := env.svc.MaybeRevert(context.Background(), "sess-1")
	if err != nil || !reverted {
		t.Fatalf("MaybeRevert() = %v, %v, want true, nil", reverted, err)
	}
	esc, _, _ = env.svc.Get(context.Background(), "sess-1")
	if esc.State != domain.TemporaryEscalationReverted || esc.AttemptCount != 1 {
		t.Fatalf("state = %q attempts = %d, want reverted/1", esc.State, esc.AttemptCount)
	}
	rec, _, _ = env.reader.GetSession(context.Background(), "sess-1")
	if rec.Harness != domain.HarnessCodex || rec.Metadata.Model != "mai-code-2" || rec.Metadata.Effort != "medium" {
		t.Fatalf("session = %s/%s/%s, want codex/mai-code-2/medium", rec.Harness, rec.Metadata.Model, rec.Metadata.Effort)
	}
	// A second revert attempt and a second done are both no-ops.
	if reverted, _ := env.svc.MaybeRevert(context.Background(), "sess-1"); reverted {
		t.Error("MaybeRevert() after reverted = true, want false")
	}
	env.advance(time.Minute)
	if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-2")); err != nil {
		t.Fatalf("second HandleReportDone() = %v", err)
	}
	if got := len(env.switcher.switches); got != 2 {
		t.Errorf("switches = %d, want exactly 2 (upgrade + revert)", got)
	}
}

func TestIdleWithoutDoneDoesNotRevert(t *testing.T) {
	env := newTestEnv(t)
	startUpgrade(t, env)
	reverted, err := env.svc.MaybeRevert(context.Background(), "sess-1")
	if err != nil || reverted {
		t.Fatalf("MaybeRevert() without done = %v, %v, want false, nil", reverted, err)
	}
}

func TestStaleDoneIsIgnored(t *testing.T) {
	t.Run("report predates escalation", func(t *testing.T) {
		env := newTestEnv(t)
		esc := startUpgrade(t, env)
		stale := doneReport(env, "rpt-old")
		stale.CreatedAt = esc.CreatedAt.Add(-time.Minute)
		if err := env.svc.HandleReportDone(context.Background(), stale); err != nil {
			t.Fatalf("HandleReportDone() = %v", err)
		}
		got, _, _ := env.svc.Get(context.Background(), "sess-1")
		if got.State != domain.TemporaryEscalationPending {
			t.Errorf("state = %q, want pending", got.State)
		}
	})
	t.Run("newer turn means newer work", func(t *testing.T) {
		env := newTestEnv(t)
		startUpgrade(t, env)
		env.reader.setTurn("conv-1", "turn-2", domain.TurnStateRunning)
		env.advance(time.Minute)
		if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-1")); err != nil {
			t.Fatalf("HandleReportDone() = %v", err)
		}
		got, _, _ := env.svc.Get(context.Background(), "sess-1")
		if got.State != domain.TemporaryEscalationPending {
			t.Errorf("state = %q, want pending", got.State)
		}
	})
	t.Run("generation replaced", func(t *testing.T) {
		env := newTestEnv(t)
		startUpgrade(t, env)
		env.reader.setGeneration("sess-1", "gen-99")
		env.advance(time.Minute)
		if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-1")); err != nil {
			t.Fatalf("HandleReportDone() = %v", err)
		}
		got, _, _ := env.svc.Get(context.Background(), "sess-1")
		if got.State != domain.TemporaryEscalationPending {
			t.Errorf("state = %q, want pending", got.State)
		}
	})
	t.Run("non-done report", func(t *testing.T) {
		env := newTestEnv(t)
		startUpgrade(t, env)
		rep := doneReport(env, "rpt-1")
		rep.State = domain.ReportCheckpoint
		if err := env.svc.HandleReportDone(context.Background(), rep); err != nil {
			t.Fatalf("HandleReportDone() = %v", err)
		}
		got, _, _ := env.svc.Get(context.Background(), "sess-1")
		if got.State != domain.TemporaryEscalationPending {
			t.Errorf("state = %q, want pending", got.State)
		}
	})
}

func TestManualSwitchSupersedes(t *testing.T) {
	env := newTestEnv(t)
	startUpgrade(t, env)
	if err := env.svc.NoteManualSwitch(context.Background(), "sess-1", domain.ProviderSwitchRequest{
		TargetHarness: domain.HarnessCodex, Model: "other-model",
	}); err != nil {
		t.Fatalf("NoteManualSwitch() = %v", err)
	}
	esc, _, _ := env.svc.Get(context.Background(), "sess-1")
	if esc.State != domain.TemporaryEscalationSuperseded {
		t.Fatalf("state = %q, want superseded", esc.State)
	}
	// A later done must not bounce back to the snapshot.
	env.advance(time.Minute)
	if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-1")); err != nil {
		t.Fatalf("HandleReportDone() = %v", err)
	}
	if reverted, _ := env.svc.MaybeRevert(context.Background(), "sess-1"); reverted {
		t.Error("reverted after supersede, want no revert")
	}
}

func TestManualTurnSettingsSupersedes(t *testing.T) {
	env := newTestEnv(t)
	startUpgrade(t, env)
	// A same-harness manual model change (the picker path) also cancels
	// the pending auto-restore.
	if err := env.svc.NoteManualTurnSettings(context.Background(), "sess-1", domain.ConversationSettings{Model: "other-model"}); err != nil {
		t.Fatalf("NoteManualTurnSettings() = %v", err)
	}
	esc, _, _ := env.svc.Get(context.Background(), "sess-1")
	if esc.State != domain.TemporaryEscalationSuperseded {
		t.Fatalf("state = %q, want superseded", esc.State)
	}
}

func TestCancelAndRestoreNow(t *testing.T) {
	env := newTestEnv(t)
	startUpgrade(t, env)
	if err := env.svc.Cancel(context.Background(), "sess-1"); err != nil {
		t.Fatalf("Cancel() = %v", err)
	}
	esc, _, _ := env.svc.Get(context.Background(), "sess-1")
	if esc.State != domain.TemporaryEscalationCancelled {
		t.Fatalf("state = %q, want cancelled", esc.State)
	}
	if err := env.svc.RestoreNow(context.Background(), "sess-1"); !errors.Is(err, ErrNoEscalation) {
		t.Errorf("RestoreNow() after cancel = %v, want ErrNoEscalation", err)
	}

	env = newTestEnv(t)
	startUpgrade(t, env)
	// RestoreNow refuses while the worker is busy...
	env.reader.setActivity("sess-1", domain.ActivityActive)
	if err := env.svc.RestoreNow(context.Background(), "sess-1"); !errors.Is(err, ErrEscalationNotIdle) {
		t.Errorf("RestoreNow() while busy = %v, want ErrEscalationNotIdle", err)
	}
	// ...and restores immediately once idle, with no done report.
	env.reader.setActivity("sess-1", domain.ActivityIdle)
	if err := env.svc.RestoreNow(context.Background(), "sess-1"); err != nil {
		t.Fatalf("RestoreNow() = %v", err)
	}
	esc, _, _ = env.svc.Get(context.Background(), "sess-1")
	if esc.State != domain.TemporaryEscalationReverted {
		t.Fatalf("state = %q, want reverted", esc.State)
	}
}

func TestRevertFailureKeepsWorkerAndRetries(t *testing.T) {
	env := newTestEnv(t)
	startUpgradeWithTask(t, env, "turn-2")
	env.reader.setTurn("conv-1", "turn-2", domain.TurnStateCompleted)
	env.switcher.failNext = 1
	env.advance(time.Minute)
	if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-1")); err != nil {
		t.Fatalf("HandleReportDone() = %v", err)
	}
	esc, _, _ := env.svc.Get(context.Background(), "sess-1")
	if esc.State != domain.TemporaryEscalationFailed || esc.LastError == "" {
		t.Fatalf("state = %q lastError = %q, want failed/nonempty", esc.State, esc.LastError)
	}
	rec, _, _ := env.reader.GetSession(context.Background(), "sess-1")
	if rec.Harness != domain.HarnessClaudeCode {
		t.Fatalf("worker moved to %s, want it preserved on claude-code", rec.Harness)
	}
	// Reconcile never retries a failed revert on its own.
	if n, err := env.svc.Reconcile(context.Background()); err != nil || n != 0 {
		t.Fatalf("Reconcile() = %d, %v, want 0, nil", n, err)
	}
	// An explicit retry succeeds.
	if err := env.svc.RetryRevert(context.Background(), "sess-1"); err != nil {
		t.Fatalf("RetryRevert() = %v", err)
	}
	esc, _, _ = env.svc.Get(context.Background(), "sess-1")
	if esc.State != domain.TemporaryEscalationReverted || esc.AttemptCount != 2 {
		t.Fatalf("state = %q attempts = %d, want reverted/2", esc.State, esc.AttemptCount)
	}
}

func TestReconcilePicksUpAwaitingIdle(t *testing.T) {
	env := newTestEnv(t)
	startUpgradeWithTask(t, env, "turn-2")
	env.reader.setActivity("sess-1", domain.ActivityActive)
	env.reader.setTurn("conv-1", "turn-2", domain.TurnStateRunning)
	env.advance(time.Minute)
	if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-1")); err != nil {
		t.Fatalf("HandleReportDone() = %v", err)
	}
	if n, _ := env.svc.Reconcile(context.Background()); n != 0 {
		t.Fatalf("Reconcile() while busy = %d, want 0", n)
	}
	env.reader.setActivity("sess-1", domain.ActivityIdle)
	env.reader.setTurn("conv-1", "turn-2", domain.TurnStateCompleted)
	if n, err := env.svc.Reconcile(context.Background()); err != nil || n != 1 {
		t.Fatalf("Reconcile() = %d, %v, want 1, nil", n, err)
	}
}

func TestRestartRecovery(t *testing.T) {
	env := newTestEnv(t)
	esc := startUpgrade(t, env)
	// Simulate a crash mid-revert: the record is stuck in reverting.
	now := env.now
	if err := esc.BeginRevert(now); err != nil {
		t.Fatalf("BeginRevert() = %v", err)
	}
	if _, err := env.store.UpdateEscalation(context.Background(), esc, domain.TemporaryEscalationPending); err != nil {
		t.Fatalf("UpdateEscalation() = %v", err)
	}
	if err := env.svc.RecoverAfterRestart(context.Background()); err != nil {
		t.Fatalf("RecoverAfterRestart() = %v", err)
	}
	got, _, _ := env.svc.Get(context.Background(), "sess-1")
	if got.State != domain.TemporaryEscalationFailed {
		t.Fatalf("state = %q, want failed", got.State)
	}

	// A merely pending escalation survives a restart untouched.
	env = newTestEnv(t)
	startUpgrade(t, env)
	if err := env.svc.RecoverAfterRestart(context.Background()); err != nil {
		t.Fatalf("RecoverAfterRestart() = %v", err)
	}
	got, _, _ = env.svc.Get(context.Background(), "sess-1")
	if got.State != domain.TemporaryEscalationPending {
		t.Fatalf("state = %q, want pending", got.State)
	}
}

func TestRevertingAlreadyOnOriginalModel(t *testing.T) {
	env := newTestEnv(t)
	esc := startUpgrade(t, env)
	// The revert switch committed, then the daemon died before marking
	// it reverted. The session and its Chat controller settings are
	// already back on the original model/effort/approval.
	rec, _, _ := env.reader.GetSession(context.Background(), "sess-1")
	rec.Harness = domain.HarnessCodex
	rec.Metadata.Model = "mai-code-2"
	rec.Metadata.Effort = "medium"
	env.reader.sessions["sess-1"] = rec
	conv, _ := env.reader.ConversationForSession(context.Background(), "sess-1")
	conv.Settings.Model = "mai-code-2"
	conv.Settings.ReasoningEffort = "medium"
	conv.Settings.ApprovalMode = domain.PermissionModeAuto
	env.reader.convs["sess-1"] = conv
	if err := esc.BeginRevert(env.now); err != nil {
		t.Fatalf("BeginRevert() = %v", err)
	}
	if _, err := env.store.UpdateEscalation(context.Background(), esc, domain.TemporaryEscalationPending); err != nil {
		t.Fatalf("UpdateEscalation() = %v", err)
	}
	reverted, err := env.svc.MaybeRevert(context.Background(), "sess-1")
	if err != nil || !reverted {
		t.Fatalf("MaybeRevert() = %v, %v, want true, nil", reverted, err)
	}
}

func TestBindWorkItemBindsNewTurn(t *testing.T) {
	env := newTestEnv(t)
	esc := startUpgrade(t, env)
	if esc.WorkItemID != "" {
		t.Fatalf("WorkItemID = %q, want unbound at Start", esc.WorkItemID)
	}
	// A completion arriving before any binding is ignored: the
	// completed turn-1 predates the upgrade.
	env.advance(time.Minute)
	if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-early")); err != nil {
		t.Fatalf("HandleReportDone() = %v", err)
	}
	got, _, _ := env.svc.Get(context.Background(), "sess-1")
	if got.State != domain.TemporaryEscalationPending {
		t.Fatalf("state = %q, want pending after unbound done", got.State)
	}
	// The first post-switch send creates turn-2; bind it.
	env.reader.setTurn("conv-1", "turn-2", domain.TurnStateRunning)
	if err := env.svc.BindWorkItem(context.Background(), "sess-1", "turn-2"); err != nil {
		t.Fatalf("BindWorkItem(turn-2) = %v", err)
	}
	got, _, _ = env.svc.Get(context.Background(), "sess-1")
	if got.WorkItemID != "turn-2" {
		t.Fatalf("WorkItemID = %q, want turn-2", got.WorkItemID)
	}
	// Re-binding the same turn is idempotent; binding a different turn
	// is refused — the escalated task is fixed at the first send.
	if err := env.svc.BindWorkItem(context.Background(), "sess-1", "turn-2"); err != nil {
		t.Errorf("BindWorkItem(turn-2) again = %v, want nil", err)
	}
	if err := env.svc.BindWorkItem(context.Background(), "sess-1", "turn-3"); err == nil {
		t.Error("BindWorkItem(turn-3) after binding turn-2 = nil, want error")
	}
	got, _, _ = env.svc.Get(context.Background(), "sess-1")
	if got.WorkItemID != "turn-2" {
		t.Fatalf("WorkItemID = %q after refused bind, want turn-2", got.WorkItemID)
	}
	// Full path: the bound turn's done, then idle, restores.
	env.advance(time.Minute)
	if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-1")); err != nil {
		t.Fatalf("HandleReportDone() = %v", err)
	}
	got, _, _ = env.svc.Get(context.Background(), "sess-1")
	if got.State != domain.TemporaryEscalationAwaitingIdle {
		t.Fatalf("state = %q, want awaiting_idle", got.State)
	}
	env.reader.setActivity("sess-1", domain.ActivityIdle)
	env.reader.setTurn("conv-1", "turn-2", domain.TurnStateCompleted)
	if reverted, err := env.svc.MaybeRevert(context.Background(), "sess-1"); err != nil || !reverted {
		t.Fatalf("MaybeRevert() = %v, %v, want true, nil", reverted, err)
	}
}

func TestPersistedDoneReplayedAfterRestart(t *testing.T) {
	t.Run("RecoverAfterRestart replays a persisted done", func(t *testing.T) {
		env := newTestEnv(t)
		startUpgradeWithTask(t, env, "turn-2")
		env.reader.setTurn("conv-1", "turn-2", domain.TurnStateCompleted)
		// Crash window: the report was committed to storage but the
		// fire-and-forget HandleReportDone goroutine never ran.
		env.reader.reports = map[domain.SessionID][]domain.ReportRecord{
			"sess-1": {doneReport(env, "rpt-1")},
		}
		if err := env.svc.RecoverAfterRestart(context.Background()); err != nil {
			t.Fatalf("RecoverAfterRestart() = %v", err)
		}
		esc, _, _ := env.svc.Get(context.Background(), "sess-1")
		if esc.State != domain.TemporaryEscalationReverted {
			t.Fatalf("state = %q, want reverted after replay", esc.State)
		}
		rec, _, _ := env.reader.GetSession(context.Background(), "sess-1")
		if rec.Harness != domain.HarnessCodex || rec.Metadata.Model != "mai-code-2" {
			t.Fatalf("session = %s/%s, want codex/mai-code-2", rec.Harness, rec.Metadata.Model)
		}
		// Replay is idempotent: a second restart recovery is a no-op.
		if err := env.svc.RecoverAfterRestart(context.Background()); err != nil {
			t.Fatalf("second RecoverAfterRestart() = %v", err)
		}
	})
	t.Run("Reconcile replays a persisted done", func(t *testing.T) {
		env := newTestEnv(t)
		startUpgradeWithTask(t, env, "turn-2")
		env.reader.setActivity("sess-1", domain.ActivityActive)
		env.reader.setTurn("conv-1", "turn-2", domain.TurnStateRunning)
		// Crash window: the report was committed to storage but the
		// fire-and-forget HandleReportDone goroutine never ran.
		env.reader.reports = map[domain.SessionID][]domain.ReportRecord{
			"sess-1": {doneReport(env, "rpt-1")},
		}
		// The sweep replays the persisted done (arming it) but does
		// not revert while the worker is busy.
		if n, err := env.svc.Reconcile(context.Background()); err != nil || n != 0 {
			t.Fatalf("Reconcile() while busy = %d, %v, want 0, nil", n, err)
		}
		esc, _, _ := env.svc.Get(context.Background(), "sess-1")
		if esc.State != domain.TemporaryEscalationAwaitingIdle {
			t.Fatalf("state = %q, want awaiting_idle after replay", esc.State)
		}
		env.reader.setActivity("sess-1", domain.ActivityIdle)
		env.reader.setTurn("conv-1", "turn-2", domain.TurnStateCompleted)
		if n, err := env.svc.Reconcile(context.Background()); err != nil || n != 1 {
			t.Fatalf("Reconcile() once idle = %d, %v, want 1, nil", n, err)
		}
	})
}

func TestRetryRevertRequiresIdle(t *testing.T) {
	env := newTestEnv(t)
	startUpgradeWithTask(t, env, "turn-2")
	env.reader.setTurn("conv-1", "turn-2", domain.TurnStateCompleted)
	env.switcher.failNext = 1
	env.advance(time.Minute)
	if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-1")); err != nil {
		t.Fatalf("HandleReportDone() = %v", err)
	}
	esc, _, _ := env.svc.Get(context.Background(), "sess-1")
	if esc.State != domain.TemporaryEscalationFailed {
		t.Fatalf("state = %q, want failed", esc.State)
	}
	switches := len(env.switcher.switches)
	// Retry while the worker is active: refused, still failed, no switch.
	env.reader.setActivity("sess-1", domain.ActivityActive)
	env.reader.setTurn("conv-1", "turn-2", domain.TurnStateRunning)
	if err := env.svc.RetryRevert(context.Background(), "sess-1"); !errors.Is(err, ErrEscalationNotIdle) {
		t.Errorf("RetryRevert() while active = %v, want ErrEscalationNotIdle", err)
	}
	// Retry with a pending interaction: refused the same way.
	env.reader.setActivity("sess-1", domain.ActivityIdle)
	env.reader.setTurn("conv-1", "turn-2", domain.TurnStateCompleted)
	env.reader.pending["conv-1"] = true
	if err := env.svc.RetryRevert(context.Background(), "sess-1"); !errors.Is(err, ErrEscalationNotIdle) {
		t.Errorf("RetryRevert() with pending interaction = %v, want ErrEscalationNotIdle", err)
	}
	esc, _, _ = env.svc.Get(context.Background(), "sess-1")
	if esc.State != domain.TemporaryEscalationFailed {
		t.Errorf("state = %q after refused retries, want failed", esc.State)
	}
	if got := len(env.switcher.switches); got != switches {
		t.Errorf("switches = %d, want %d (no switch started)", got, switches)
	}
	// Once idle, the explicit retry succeeds and stays retryable before.
	delete(env.reader.pending, "conv-1")
	if err := env.svc.RetryRevert(context.Background(), "sess-1"); err != nil {
		t.Fatalf("RetryRevert() once idle = %v", err)
	}
	esc, _, _ = env.svc.Get(context.Background(), "sess-1")
	if esc.State != domain.TemporaryEscalationReverted {
		t.Fatalf("state = %q, want reverted", esc.State)
	}
}

func TestDelayedRevertSupersededByNewerWork(t *testing.T) {
	t.Run("newer turn takes over before the sweep", func(t *testing.T) {
		env := newTestEnv(t)
		startUpgradeWithTask(t, env, "turn-2")
		env.reader.setActivity("sess-1", domain.ActivityActive)
		env.reader.setTurn("conv-1", "turn-2", domain.TurnStateRunning)
		env.advance(time.Minute)
		if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-1")); err != nil {
			t.Fatalf("HandleReportDone() = %v", err)
		}
		// A newer turn starts and finishes before the reconciler runs.
		env.reader.setActivity("sess-1", domain.ActivityIdle)
		env.reader.setTurn("conv-1", "turn-3", domain.TurnStateCompleted)
		if n, err := env.svc.Reconcile(context.Background()); err != nil || n != 0 {
			t.Fatalf("Reconcile() = %d, %v, want 0, nil", n, err)
		}
		esc, _, _ := env.svc.Get(context.Background(), "sess-1")
		if esc.State != domain.TemporaryEscalationSuperseded {
			t.Fatalf("state = %q, want superseded", esc.State)
		}
		rec, _, _ := env.reader.GetSession(context.Background(), "sess-1")
		if rec.Harness != domain.HarnessClaudeCode || rec.Metadata.Model != "sol-6.2" {
			t.Fatalf("session = %s/%s, want it still on claude-code/sol-6.2", rec.Harness, rec.Metadata.Model)
		}
		if got := len(env.switcher.switches); got != 1 {
			t.Errorf("switches = %d, want 1 (upgrade only)", got)
		}
	})
	t.Run("replaced controller generation takes over", func(t *testing.T) {
		env := newTestEnv(t)
		startUpgradeWithTask(t, env, "turn-2")
		env.reader.setActivity("sess-1", domain.ActivityActive)
		env.reader.setTurn("conv-1", "turn-2", domain.TurnStateRunning)
		env.advance(time.Minute)
		if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-1")); err != nil {
			t.Fatalf("HandleReportDone() = %v", err)
		}
		env.reader.setGeneration("sess-1", "gen-99")
		env.reader.setActivity("sess-1", domain.ActivityIdle)
		env.reader.setTurn("conv-1", "turn-2", domain.TurnStateCompleted)
		if n, err := env.svc.Reconcile(context.Background()); err != nil || n != 0 {
			t.Fatalf("Reconcile() = %d, %v, want 0, nil", n, err)
		}
		esc, _, _ := env.svc.Get(context.Background(), "sess-1")
		if esc.State != domain.TemporaryEscalationSuperseded {
			t.Fatalf("state = %q, want superseded", esc.State)
		}
	})
}

func TestRestoreConfirmationChecksControllerSettings(t *testing.T) {
	t.Run("cross-harness revert with wrong controller settings is not reverted", func(t *testing.T) {
		env := newTestEnv(t)
		startUpgradeWithTask(t, env, "turn-2")
		env.reader.setTurn("conv-1", "turn-2", domain.TurnStateCompleted)
		env.advance(time.Minute)
		// The revert switch lands the session-level model but leaves
		// the Chat controller on the upgraded settings.
		env.switcher.preserveConvSettings = true
		if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-1")); err != nil {
			t.Fatalf("HandleReportDone() = %v", err)
		}
		esc, _, _ := env.svc.Get(context.Background(), "sess-1")
		if esc.State != domain.TemporaryEscalationFailed || esc.LastError == "" {
			t.Fatalf("state = %q lastError = %q, want failed/nonempty", esc.State, esc.LastError)
		}
		// With the settings fixed, the explicit retry restores.
		env.switcher.preserveConvSettings = false
		if err := env.svc.RetryRevert(context.Background(), "sess-1"); err != nil {
			t.Fatalf("RetryRevert() = %v", err)
		}
		esc, _, _ = env.svc.Get(context.Background(), "sess-1")
		if esc.State != domain.TemporaryEscalationReverted {
			t.Fatalf("state = %q, want reverted", esc.State)
		}
	})
	t.Run("same-harness revert with wrong effort is not reverted", func(t *testing.T) {
		env := newTestEnv(t)
		if _, err := env.svc.Start(context.Background(), "sess-1", domain.ProviderSwitchRequest{
			TargetHarness: domain.HarnessCodex, Model: "mai-code-3", Effort: "high",
		}); err != nil {
			t.Fatalf("Start() = %v", err)
		}
		env.reader.setTurn("conv-1", "turn-2", domain.TurnStateRunning)
		if err := env.svc.BindWorkItem(context.Background(), "sess-1", "turn-2"); err != nil {
			t.Fatalf("BindWorkItem() = %v", err)
		}
		env.reader.setTurn("conv-1", "turn-2", domain.TurnStateCompleted)
		env.advance(time.Minute)
		// The revert leaves the Chat controller on the wrong effort.
		env.switcher.preserveConvSettings = true
		if err := env.svc.HandleReportDone(context.Background(), doneReport(env, "rpt-1")); err != nil {
			t.Fatalf("HandleReportDone() = %v", err)
		}
		esc, _, _ := env.svc.Get(context.Background(), "sess-1")
		if esc.State != domain.TemporaryEscalationFailed {
			t.Fatalf("state = %q, want failed", esc.State)
		}
		env.switcher.preserveConvSettings = false
		if err := env.svc.RetryRevert(context.Background(), "sess-1"); err != nil {
			t.Fatalf("RetryRevert() = %v", err)
		}
		esc, _, _ = env.svc.Get(context.Background(), "sess-1")
		if esc.State != domain.TemporaryEscalationReverted {
			t.Fatalf("state = %q, want reverted", esc.State)
		}
	})
}
