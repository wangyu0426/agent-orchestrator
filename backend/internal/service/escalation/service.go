// Package escalation implements manual temporary model escalation for
// worker sessions (issue #8).
//
// The owner explicitly upgrades one session to a higher-capability model
// (optionally a different harness) for the current task only. The service
// snapshots the original harness/model/effort/launch settings, performs
// the switch through the existing provider-switch paths, and — after both
// an authoritative, correlated completion signal (ao report --done) and a
// safe idle boundary — restores the original model exactly once.
//
// One level, one shot: no nesting, no automatic upgrades, no
// recommendations, no budget heuristics. A newer explicit owner choice
// supersedes the pending auto-restore, so AO never bounces back
// unexpectedly. On revert failure the worker and worktree are preserved,
// the escalation stays in a retryable failed state, and an explicit retry
// or manual recovery is required. The service never starts two
// controllers for a session: every switch reuses the existing
// provider-switch locking and generation fencing.
package escalation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Sentinels for the states a caller can act on.
var (
	ErrSessionNotFound        = errors.New("escalation: session not found")
	ErrEscalationActive       = errors.New("escalation: a temporary escalation is already active for this session")
	ErrNoEscalation           = errors.New("escalation: no temporary escalation for this session")
	ErrEscalationBusy         = errors.New("escalation: the worker is still working")
	ErrEscalationNotIdle      = errors.New("escalation: the worker has not reached a safe idle boundary")
	ErrEscalationUnsupported  = errors.New("escalation: temporary escalation is not supported for this session")
	ErrPendingInteraction     = errors.New("escalation: answer the pending provider interaction before switching")
	ErrEscalationStateChanged = errors.New("escalation: the escalation changed while this operation was running")
)

// Reader is the session state the service snapshots and fences on. Its
// methods match the SQLite store so the daemon can pass the store
// directly.
type Reader interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	ConversationForSession(ctx context.Context, id domain.SessionID) (domain.ConversationRecord, error)
	LatestConversationTurn(ctx context.Context, conversationID string) (domain.ConversationTurn, error)
	HasPendingConversationInteractions(ctx context.Context, conversationID string) (bool, error)
	GetActiveSessionInterfaceTransition(ctx context.Context, id domain.SessionID) (domain.SessionInterfaceTransition, bool, error)
	SessionLaunchOptions(ctx context.Context, id domain.SessionID) (domain.AgentLaunchOptions, error)
}

// Store is the durable escalation record. UpdateEscalation is a
// compare-and-swap on the previous state: it reports false when another
// writer moved the record first, which is what makes the automatic
// restore happen exactly once.
type Store interface {
	GetEscalation(ctx context.Context, id domain.SessionID) (*domain.TemporaryEscalation, bool, error)
	CreateEscalation(ctx context.Context, esc *domain.TemporaryEscalation) error
	UpdateEscalation(ctx context.Context, esc *domain.TemporaryEscalation, from domain.TemporaryEscalationState) (bool, error)
	DeleteEscalation(ctx context.Context, id domain.SessionID) error
	ListActiveEscalations(ctx context.Context) ([]*domain.TemporaryEscalation, error)
}

// Switcher performs the actual model/harness changes. It is the existing
// provider-switch surface: SwitchProvider already routes Chat and TUI
// sessions to their own switch paths, and SetTurnSettings is the
// same-harness Chat model change.
type Switcher interface {
	SwitchProvider(ctx context.Context, id domain.SessionID, req domain.ProviderSwitchRequest) error
	SetTurnSettings(ctx context.Context, id domain.SessionID, settings domain.ConversationSettings) (domain.ConversationSettings, error)
	PreflightChat(ctx context.Context, harness domain.AgentHarness, permissions ports.PermissionMode) error
}

// Deps configures a Service.
type Deps struct {
	Reader   Reader
	Store    Store
	Switcher Switcher
	Now      func() time.Time
	NewID    func() string
}

// Service orchestrates temporary escalations for one daemon.
type Service struct {
	reader   Reader
	store    Store
	switcher Switcher
	now      func() time.Time
	newID    func() string

	mu        sync.Mutex
	locks     map[domain.SessionID]*sync.Mutex
	switching map[domain.SessionID]bool
}

// New constructs a Service.
func New(d Deps) *Service {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	newID := d.NewID
	if newID == nil {
		newID = func() string { return "esc_" + uuid.NewString() }
	}
	return &Service{
		reader:    d.Reader,
		store:     d.Store,
		switcher:  d.Switcher,
		now:       now,
		newID:     newID,
		locks:     map[domain.SessionID]*sync.Mutex{},
		switching: map[domain.SessionID]bool{},
	}
}

func (s *Service) lock(id domain.SessionID) func() {
	s.mu.Lock()
	l, ok := s.locks[id]
	if !ok {
		l = &sync.Mutex{}
		s.locks[id] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (s *Service) markSwitching(id domain.SessionID, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		s.switching[id] = true
	} else {
		delete(s.switching, id)
	}
}

func (s *Service) isSwitching(id domain.SessionID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.switching[id]
}

// fenceFor is the controller fence for a session: the Chat controller
// generation, or the terminal runtime launch id for TUI sessions.
func fenceFor(rec domain.SessionRecord) string {
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		return rec.Metadata.ControllerGeneration
	}
	return rec.Metadata.RuntimeLaunchID
}

func (s *Service) save(ctx context.Context, esc *domain.TemporaryEscalation, from domain.TemporaryEscalationState) error {
	if s.store == nil {
		return errors.New("escalation: store is required")
	}
	ok, err := s.store.UpdateEscalation(ctx, esc, from)
	if err != nil {
		return err
	}
	if !ok {
		return ErrEscalationStateChanged
	}
	return nil
}

// Get returns the latest escalation for a session, terminal or not, so
// the UI can show what will (or did) happen. found is false when the
// session never had one.
func (s *Service) Get(ctx context.Context, id domain.SessionID) (*domain.TemporaryEscalation, bool, error) {
	if s.store == nil {
		return nil, false, errors.New("escalation: store is required")
	}
	return s.store.GetEscalation(ctx, id)
}

// Start snapshots the session's current model, persists a pending
// escalation, and switches to the target. The worker and worktree are
// preserved on failure: the switch paths restore the source, and the
// pending record is deleted so no auto-restore can fire for a switch
// that never happened.
func (s *Service) Start(ctx context.Context, id domain.SessionID, target domain.ProviderSwitchRequest) (*domain.TemporaryEscalation, error) {
	if s.reader == nil || s.store == nil || s.switcher == nil {
		return nil, errors.New("escalation: reader, store and switcher are required")
	}
	if target.TargetHarness == "" {
		return nil, fmt.Errorf("%w: target provider is required", ErrEscalationUnsupported)
	}
	unlock := s.lock(id)
	defer unlock()

	rec, found, err := s.reader.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrSessionNotFound
	}
	if rec.IsTerminated {
		return nil, fmt.Errorf("%w: terminated sessions must be restored before switching", ErrEscalationUnsupported)
	}
	mode := domain.NormalizeSessionMode(rec.Mode)
	if existing, found, err := s.store.GetEscalation(ctx, id); err != nil {
		return nil, err
	} else if found && (existing.State.Active() || existing.State == domain.TemporaryEscalationFailed) {
		return nil, fmt.Errorf("%w: resolve the %s escalation first", ErrEscalationActive, existing.State)
	}
	if target.TargetHarness == rec.Harness && target.Model == rec.Metadata.Model && target.Effort == rec.Metadata.Effort {
		return nil, fmt.Errorf("%w: the session is already on the requested model", ErrEscalationUnsupported)
	}
	// Same-harness model changes are a live Chat controller setting. A
	// TUI model's launch choices only take effect at start, and AO has no
	// same-harness TUI relaunch-with-new-model path; refuse rather than
	// pretend the switch happened.
	sameHarness := target.TargetHarness == rec.Harness
	if sameHarness && mode != domain.SessionModeChat {
		return nil, fmt.Errorf("%w: same-harness temporary model changes are only supported for Chat sessions", ErrEscalationUnsupported)
	}
	// Idle-only, like every provider switch: never silently terminate a
	// tool call or an in-flight turn.
	if rec.Activity.State != domain.ActivityIdle {
		return nil, fmt.Errorf("%w: activity is %q", ErrEscalationBusy, rec.Activity.State)
	}
	conv, convErr := s.reader.ConversationForSession(ctx, id)
	if convErr != nil {
		// TUI sessions may have no chat conversation yet; the escalation
		// still binds to the session and its fence.
		conv = domain.ConversationRecord{}
	}
	if conv.ID != "" {
		if pending, err := s.reader.HasPendingConversationInteractions(ctx, conv.ID); err != nil {
			return nil, err
		} else if pending {
			return nil, ErrPendingInteraction
		}
	}
	if _, active, err := s.reader.GetActiveSessionInterfaceTransition(ctx, id); err != nil {
		return nil, err
	} else if active {
		return nil, fmt.Errorf("%w: an interface transition is already in progress", ErrEscalationBusy)
	}
	launch, err := s.reader.SessionLaunchOptions(ctx, id)
	if err != nil {
		return nil, err
	}
	// Preflight both directions up front for Chat: a restore that cannot
	// start must fail here, while the original model still runs, not at
	// revert time. TUI preflight happens inside its switch.
	if mode == domain.SessionModeChat {
		permission := target.PermissionMode
		if permission == "" {
			permission = conv.Settings.ApprovalMode
		}
		if !sameHarness {
			if err := s.switcher.PreflightChat(ctx, target.TargetHarness, permission); err != nil {
				return nil, fmt.Errorf("escalation target preflight failed: %w", err)
			}
		}
		if err := s.switcher.PreflightChat(ctx, rec.Harness, conv.Settings.ApprovalMode); err != nil {
			return nil, fmt.Errorf("escalation restore preflight failed: %w", err)
		}
	}

	now := s.now().UTC()
	workItemID := ""
	if conv.ID != "" {
		if turn, err := s.reader.LatestConversationTurn(ctx, conv.ID); err == nil {
			workItemID = turn.ID
		} else if !errors.Is(err, domain.ErrNoConversationTurn) {
			return nil, err
		}
	}
	permission := conv.Settings.ApprovalMode
	if permission == "" {
		permission = rec.Metadata.Permissions
	}
	esc := &domain.TemporaryEscalation{
		ID:               s.newID(),
		SessionID:        id,
		ConversationID:   conv.ID,
		WorkItemID:       workItemID,
		GenerationBefore: fenceFor(rec),
		Mode:             mode,
		Original: domain.ModelSnapshot{
			Harness:            rec.Harness,
			Model:              rec.Metadata.Model,
			Effort:             rec.Metadata.Effort,
			ConversationModel:  conv.Settings.Model,
			ConversationEffort: conv.Settings.ReasoningEffort,
			PermissionMode:     permission,
			FastMode:           launch.FastMode,
			FastModeOff:        launch.FastModeOff,
			Ultracode:          launch.Ultracode,
			AutoCompactWindow:  launch.AutoCompactWindow,
		},
		TargetHarness:           target.TargetHarness,
		TargetModel:             target.Model,
		TargetEffort:            target.Effort,
		TargetFastMode:          target.FastMode,
		TargetUltracode:         target.Ultracode,
		TargetAutoCompactWindow: target.AutoCompactWindow,
		TargetPermissionMode:    target.PermissionMode,
		State:                   domain.TemporaryEscalationPending,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	if err := esc.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.CreateEscalation(ctx, esc); err != nil {
		return nil, err
	}
	s.markSwitching(id, true)
	switchErr := s.performSwitch(ctx, id, esc.UpgradeRequest(), esc, true)
	s.markSwitching(id, false)
	if switchErr != nil {
		// The switch restored the source; drop the pending record so
		// nothing can auto-restore a switch that never happened.
		_ = s.store.DeleteEscalation(ctx, id)
		return nil, fmt.Errorf("temporary escalation switch failed: %w", switchErr)
	}
	// The switch rotated the fence; record the post-switch value so a
	// completion naming it still correlates. Best effort: the escalation
	// stays usable with the pre-switch fence even if this write loses a
	// race, and RestoreNow does not depend on it.
	if after, found, err := s.reader.GetSession(ctx, id); err == nil && found {
		esc.GenerationAfter = fenceFor(after)
		esc.UpdatedAt = s.now().UTC()
		if _, err := s.store.UpdateEscalation(ctx, esc, domain.TemporaryEscalationPending); err != nil {
			return esc, fmt.Errorf("temporary escalation started but its post-switch fence could not be saved: %w", err)
		}
	}
	return esc, nil
}

// performSwitch runs one provider switch for an escalation. upgrade
// selects the upgrade request shape; otherwise the revert request is
// used. Same-harness Chat sessions change the live model instead of
// starting a new provider.
func (s *Service) performSwitch(ctx context.Context, id domain.SessionID, req domain.ProviderSwitchRequest, esc *domain.TemporaryEscalation, upgrade bool) error {
	if req.TargetHarness == "" {
		return fmt.Errorf("%w: target provider is required", ErrEscalationUnsupported)
	}
	rec, found, err := s.reader.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return ErrSessionNotFound
	}
	if req.TargetHarness == rec.Harness && domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		settings := domain.ConversationSettings{
			Model:           req.Model,
			ReasoningEffort: req.Effort,
			ApprovalMode:    req.PermissionMode,
		}
		if settings.ApprovalMode == "" {
			settings.ApprovalMode = esc.Original.PermissionMode
		}
		if req.TargetHarness == domain.HarnessClaudeCode {
			settings.AutoCompactWindow = req.AutoCompactWindow
		}
		_, err := s.switcher.SetTurnSettings(ctx, id, settings)
		return err
	}
	req.IdempotencyKey = "temporary-escalation:" + esc.ID
	if !upgrade {
		req.IdempotencyKey += ":revert"
	}
	return s.switcher.SwitchProvider(ctx, id, req)
}

// isSafeIdle reports whether the session has reached a boundary where a
// revert is safe: idle activity, no pending provider interaction, no
// running turn, and no interface transition in flight.
func (s *Service) isSafeIdle(ctx context.Context, esc *domain.TemporaryEscalation) (bool, error) {
	rec, found, err := s.reader.GetSession(ctx, esc.SessionID)
	if err != nil {
		return false, err
	}
	if !found || rec.IsTerminated || rec.Activity.State != domain.ActivityIdle {
		return false, nil
	}
	if _, active, err := s.reader.GetActiveSessionInterfaceTransition(ctx, esc.SessionID); err != nil {
		return false, err
	} else if active {
		return false, nil
	}
	if esc.ConversationID == "" {
		return true, nil
	}
	if pending, err := s.reader.HasPendingConversationInteractions(ctx, esc.ConversationID); err != nil {
		return false, err
	} else if pending {
		return false, nil
	}
	turn, err := s.reader.LatestConversationTurn(ctx, esc.ConversationID)
	if err != nil {
		if errors.Is(err, domain.ErrNoConversationTurn) {
			return true, nil
		}
		return false, err
	}
	return turn.State.Terminal(), nil
}

// HandleReportDone is the daemon's report hook for ao report --done. A
// completion report alone never reverts anything: it only records the
// correlated done, and the revert still waits for safe idle.
func (s *Service) HandleReportDone(ctx context.Context, report domain.ReportRecord) error {
	if report.State != domain.ReportDone {
		return nil
	}
	if s.reader == nil || s.store == nil {
		return errors.New("escalation: reader and store are required")
	}
	unlock := s.lock(report.SessionID)
	defer unlock()
	return s.handleReportDoneLocked(ctx, report)
}

func (s *Service) handleReportDoneLocked(ctx context.Context, report domain.ReportRecord) error {
	esc, found, err := s.store.GetEscalation(ctx, report.SessionID)
	if err != nil {
		return err
	}
	if !found || !esc.State.Active() {
		return nil
	}
	if esc.DoneReportID != "" {
		return nil // duplicate done: the first one already armed the revert.
	}
	if !report.CreatedAt.IsZero() && report.CreatedAt.Before(esc.CreatedAt) {
		return nil // stale: reported before the escalation existed.
	}
	// Fence on the controller generation: a done from an older controller
	// epoch (or a session that has since been replaced) cannot revert a
	// newer task.
	rec, found, err := s.reader.GetSession(ctx, report.SessionID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	fence := fenceFor(rec)
	if fence != esc.GenerationBefore && (esc.GenerationAfter == "" || fence != esc.GenerationAfter) {
		return nil
	}
	// Fence on the work item: only a completion for the escalated turn
	// counts. A newer turn means newer work, which must not be reverted.
	if esc.WorkItemID != "" {
		if esc.ConversationID == "" {
			return nil
		}
		if !s.turnIsWorkItem(ctx, esc) {
			return nil
		}
	}
	now := s.now().UTC()
	if !s.persistDone(ctx, esc, report.ID, now) {
		return nil // lost a race with cancel/supersede, or the store write failed: the report is ignored.
	}
	_, _ = s.attemptRevertLocked(ctx, esc)
	return nil
}

// persistDone records the completion on the escalation and persists it.
// False means the escalation already moved on (cancel/supersede race) or
// the store write failed; either way no revert is attempted from a stale
// or unpersisted record.
func (s *Service) persistDone(ctx context.Context, esc *domain.TemporaryEscalation, reportID string, now time.Time) bool {
	if esc.MarkDone(reportID, now) != nil {
		return false
	}
	return s.save(ctx, esc, domain.TemporaryEscalationPending) == nil
}

// turnIsWorkItem reports whether the session's latest turn is still the
// turn the escalation was started for. A missing turn fails closed: no
// correlation, no revert.
func (s *Service) turnIsWorkItem(ctx context.Context, esc *domain.TemporaryEscalation) bool {
	turn, err := s.reader.LatestConversationTurn(ctx, esc.ConversationID)
	if err != nil {
		return false
	}
	return turn.ID == esc.WorkItemID
}

// attemptRevertLocked tries the automatic restore for an escalation that
// has a recorded completion. It returns true when the session is back on
// its original model.
func (s *Service) attemptRevertLocked(ctx context.Context, esc *domain.TemporaryEscalation) (bool, error) {
	// Re-read: the caller may hold a stale copy.
	fresh, found, err := s.store.GetEscalation(ctx, esc.SessionID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	esc = fresh
	switch esc.State {
	case domain.TemporaryEscalationAwaitingIdle:
		// Fall through to the idle check below.
	case domain.TemporaryEscalationReverting:
		// A previous attempt died mid-flight. If the session is already
		// back on the original model, the revert landed; otherwise it
		// failed and waits for an explicit retry.
		if s.onOriginalModel(ctx, esc) {
			return s.finishRevert(ctx, esc)
		}
		return false, s.failRevert(ctx, esc, errors.New("revert was interrupted before it could be confirmed"))
	default:
		return false, nil
	}
	idle, err := s.isSafeIdle(ctx, esc)
	if err != nil {
		return false, err
	}
	if !idle {
		return false, nil
	}
	return s.doRevertLocked(ctx, esc)
}

// onOriginalModel reports whether the session already runs the snapshot.
func (s *Service) onOriginalModel(ctx context.Context, esc *domain.TemporaryEscalation) bool {
	rec, found, err := s.reader.GetSession(ctx, esc.SessionID)
	if err != nil || !found {
		return false
	}
	return rec.Harness == esc.Original.Harness && rec.Metadata.Model == esc.Original.Model
}

// doRevertLocked moves an awaiting-idle (or owner-approved) escalation
// through the revert switch. Callers must hold the session lock.
func (s *Service) doRevertLocked(ctx context.Context, esc *domain.TemporaryEscalation) (bool, error) {
	now := s.now().UTC()
	from := esc.State
	if err := esc.BeginRevert(now); err != nil {
		return false, err
	}
	if err := s.save(ctx, esc, from); err != nil {
		return false, err
	}
	return s.revertSwitch(ctx, esc)
}

// revertSwitch performs the revert provider switch for an escalation
// already in state reverting, and records the outcome. The worker and
// worktree are preserved on failure.
func (s *Service) revertSwitch(ctx context.Context, esc *domain.TemporaryEscalation) (bool, error) {
	s.markSwitching(esc.SessionID, true)
	err := s.performSwitch(ctx, esc.SessionID, esc.RevertRequest(), esc, false)
	s.markSwitching(esc.SessionID, false)
	if err != nil {
		if errors.Is(err, domain.ErrProviderAlreadyActive) && s.onOriginalModel(ctx, esc) {
			// The restore already landed (a retry after the switch
			// committed but the confirmation was lost).
			return s.finishRevert(ctx, esc)
		}
		return false, s.failRevert(ctx, esc, fmt.Errorf("temporary escalation revert failed (worker and worktree preserved): %w", err))
	}
	// The switch committed; confirm the session really runs the original
	// before declaring the revert done.
	if !s.onOriginalModel(ctx, esc) {
		return false, s.failRevert(ctx, esc, errors.New("revert switch completed but the session is not on the original model"))
	}
	return s.finishRevert(ctx, esc)
}

// failRevert records a failed revert attempt and returns the failure.
func (s *Service) failRevert(ctx context.Context, esc *domain.TemporaryEscalation, err error) error {
	now := s.now().UTC()
	if markErr := esc.MarkFailed(now, err); markErr == nil {
		_ = s.save(ctx, esc, domain.TemporaryEscalationReverting)
	}
	return err
}

func (s *Service) finishRevert(ctx context.Context, esc *domain.TemporaryEscalation) (bool, error) {
	now := s.now().UTC()
	if err := esc.MarkReverted(now); err != nil {
		return false, err
	}
	if err := s.save(ctx, esc, domain.TemporaryEscalationReverting); err != nil {
		return false, err
	}
	return true, nil
}

// MaybeRevert attempts the automatic restore for one session. It is a
// no-op (false, nil) when nothing is due: no escalation, no recorded
// completion, or not idle yet.
func (s *Service) MaybeRevert(ctx context.Context, id domain.SessionID) (bool, error) {
	if s.reader == nil || s.store == nil {
		return false, errors.New("escalation: reader and store are required")
	}
	unlock := s.lock(id)
	defer unlock()
	esc, found, err := s.store.GetEscalation(ctx, id)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	return s.attemptRevertLocked(ctx, esc)
}

// RestoreNow restores the original model immediately, but only at a safe
// idle point. It does not wait for a completion report.
func (s *Service) RestoreNow(ctx context.Context, id domain.SessionID) error {
	if s.reader == nil || s.store == nil {
		return errors.New("escalation: reader and store are required")
	}
	unlock := s.lock(id)
	defer unlock()
	esc, found, err := s.store.GetEscalation(ctx, id)
	if err != nil {
		return err
	}
	if !found || !esc.State.Active() {
		return ErrNoEscalation
	}
	if esc.State == domain.TemporaryEscalationReverting {
		return fmt.Errorf("%w: a revert is already in progress", ErrEscalationBusy)
	}
	idle, err := s.isSafeIdle(ctx, esc)
	if err != nil {
		return err
	}
	if !idle {
		return ErrEscalationNotIdle
	}
	_, err = s.doRevertLocked(ctx, esc)
	return err
}

// RetryRevert retries a failed revert once more, on explicit request.
func (s *Service) RetryRevert(ctx context.Context, id domain.SessionID) error {
	if s.reader == nil || s.store == nil {
		return errors.New("escalation: reader and store are required")
	}
	unlock := s.lock(id)
	defer unlock()
	esc, found, err := s.store.GetEscalation(ctx, id)
	if err != nil {
		return err
	}
	if !found || esc.State != domain.TemporaryEscalationFailed {
		return ErrNoEscalation
	}
	now := s.now().UTC()
	if err := esc.RetryRevert(now); err != nil {
		return err
	}
	if err := s.save(ctx, esc, domain.TemporaryEscalationFailed); err != nil {
		return err
	}
	_, err = s.revertSwitch(ctx, esc)
	return err
}

// Cancel cancels a pending auto-restore. A revert already in flight is
// left to finish.
func (s *Service) Cancel(ctx context.Context, id domain.SessionID) error {
	if s.store == nil {
		return errors.New("escalation: store is required")
	}
	unlock := s.lock(id)
	defer unlock()
	esc, found, err := s.store.GetEscalation(ctx, id)
	if err != nil {
		return err
	}
	if !found || !esc.State.Active() {
		return ErrNoEscalation
	}
	now := s.now().UTC()
	from := esc.State
	if err := esc.MarkCancelled(now); err != nil {
		return err
	}
	return s.save(ctx, esc, from)
}

// Reconcile attempts the automatic restore for every escalation that has
// a recorded completion and is now idle. The daemon runs it on a sweep
// (completions and idle rarely arrive together) and on demand. It never
// retries a failed revert on its own.
func (s *Service) Reconcile(ctx context.Context) (int, error) {
	if s.reader == nil || s.store == nil {
		return 0, errors.New("escalation: reader and store are required")
	}
	active, err := s.store.ListActiveEscalations(ctx)
	if err != nil {
		return 0, err
	}
	reverted := 0
	var errs []error
	for _, esc := range active {
		if esc.State != domain.TemporaryEscalationAwaitingIdle && esc.State != domain.TemporaryEscalationReverting {
			continue
		}
		unlock := s.lock(esc.SessionID)
		done, err := s.attemptRevertLocked(ctx, esc)
		unlock()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if done {
			reverted++
		}
	}
	return reverted, errors.Join(errs...)
}

// RecoverAfterRestart re-arms escalations that survived a daemon
// restart. Pending and awaiting-idle escalations keep waiting; a revert
// that was in flight becomes failed instead of being silently retried.
func (s *Service) RecoverAfterRestart(ctx context.Context) error {
	if s.store == nil {
		return errors.New("escalation: store is required")
	}
	active, err := s.store.ListActiveEscalations(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, esc := range active {
		if esc.State != domain.TemporaryEscalationReverting {
			continue
		}
		unlock := s.lock(esc.SessionID)
		now := s.now().UTC()
		if err := esc.MarkInterruptedByRestart(now); err == nil {
			if err := s.save(ctx, esc, domain.TemporaryEscalationReverting); err != nil {
				errs = append(errs, err)
			}
		}
		unlock()
	}
	return errors.Join(errs...)
}

// RunReconciler sweeps for due restores until ctx is done. It returns a
// channel closed when the loop exits.
func (s *Service) RunReconciler(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = s.Reconcile(ctx)
			}
		}
	}()
	return done
}

// NoteManualSwitch records an owner's explicit provider switch. Any
// manual switch while an escalation is active supersedes the pending
// auto-restore — except AO's own revert, and except switches this
// service is performing itself (the switching guard). It implements
// domain.ManualSwitchObserver.
func (s *Service) NoteManualSwitch(ctx context.Context, id domain.SessionID, req domain.ProviderSwitchRequest) error {
	if s.store == nil {
		return nil
	}
	if s.isSwitching(id) {
		return nil
	}
	unlock := s.lock(id)
	defer unlock()
	esc, found, err := s.store.GetEscalation(ctx, id)
	if err != nil || !found {
		return nil
	}
	if esc.State.Terminal() {
		return nil
	}
	if esc.State == domain.TemporaryEscalationReverting && esc.IsRevertRequest(req) {
		return nil
	}
	now := s.now().UTC()
	from := esc.State
	if err := esc.MarkSuperseded(now); err != nil {
		return nil
	}
	_ = s.save(ctx, esc, from)
	return nil
}

// NoteManualTurnSettings records an owner's explicit same-harness model
// change. Like a provider switch, it supersedes the pending auto-restore
// unless this service is performing the change itself.
func (s *Service) NoteManualTurnSettings(ctx context.Context, id domain.SessionID, _ domain.ConversationSettings) error {
	if s.store == nil {
		return nil
	}
	if s.isSwitching(id) {
		return nil
	}
	unlock := s.lock(id)
	defer unlock()
	esc, found, err := s.store.GetEscalation(ctx, id)
	if err != nil || !found {
		return nil
	}
	if esc.State.Terminal() || esc.State == domain.TemporaryEscalationReverting {
		return nil
	}
	now := s.now().UTC()
	from := esc.State
	if err := esc.MarkSuperseded(now); err != nil {
		return nil
	}
	_ = s.save(ctx, esc, from)
	return nil
}
