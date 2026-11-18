package domain

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Temporary model escalation (issue #8).
//
// A temporary escalation is a one-level, one-shot override: the owner
// manually moves a session to a higher-capability model (optionally a
// different harness) for the current task only. AO snapshots the exact
// original harness/model/effort/launch settings, and after the task
// completes (an authoritative, correlated completion signal) and the worker
// reaches a safe idle boundary, AO restores the original model exactly
// once. There is no nesting, no automatic upgrade, no recommendation and
// no budget heuristic.
//
// Persistence: the record is stored as JSON in the sessions table
// (temporary_escalation column) with its state mirrored in the
// temporary_escalation_state column so transitions can CAS on state and
// boot recovery can list active overrides. See
// backend/internal/storage/sqlite.

// TemporaryEscalationState is the lifecycle of one temporary escalation.
type TemporaryEscalationState string

// Temporary escalation states. pending/awaiting_idle/reverting are the
// active states; reverted/superseded/cancelled are terminal; failed is
// stable and needs an explicit retry or manual recovery action.
const (
	TemporaryEscalationPending      TemporaryEscalationState = "pending"
	TemporaryEscalationAwaitingIdle TemporaryEscalationState = "awaiting_idle"
	TemporaryEscalationReverting    TemporaryEscalationState = "reverting"
	TemporaryEscalationReverted     TemporaryEscalationState = "reverted"
	TemporaryEscalationFailed       TemporaryEscalationState = "failed"
	TemporaryEscalationSuperseded   TemporaryEscalationState = "superseded"
	TemporaryEscalationCancelled    TemporaryEscalationState = "cancelled"
)

// Valid reports whether s is a known escalation state.
func (s TemporaryEscalationState) Valid() bool {
	switch s {
	case TemporaryEscalationPending, TemporaryEscalationAwaitingIdle,
		TemporaryEscalationReverting, TemporaryEscalationReverted,
		TemporaryEscalationFailed, TemporaryEscalationSuperseded,
		TemporaryEscalationCancelled:
		return true
	default:
		return false
	}
}

// Active reports whether the escalation still owns the session's model
// choice: it may still revert on its own.
func (s TemporaryEscalationState) Active() bool {
	switch s {
	case TemporaryEscalationPending, TemporaryEscalationAwaitingIdle, TemporaryEscalationReverting:
		return true
	default:
		return false
	}
}

// Terminal reports whether the escalation will never act again on its own.
// Failed is not terminal: it waits for an explicit retry or manual
// recovery, but it never reverts by itself.
func (s TemporaryEscalationState) Terminal() bool {
	switch s {
	case TemporaryEscalationReverted, TemporaryEscalationSuperseded, TemporaryEscalationCancelled:
		return true
	default:
		return false
	}
}

// ModelSnapshot is the exact original launch configuration an escalation
// restores. It covers both the session-level facts (what the session was
// spawned/resolved with) and the conversation-level turn settings (what
// the running controller was using), because the two can differ after a
// provider switch: Metadata.Model is the spawn-resolved model, while
// ConversationSettings.Model is what the chat controller runs.
type ModelSnapshot struct {
	Harness AgentHarness `json:"harness"`
	// Model and Effort are the session metadata values (spawn-resolved).
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// ConversationModel and ConversationEffort are the running chat
	// controller's turn settings. Empty means the provider default.
	ConversationModel  string `json:"conversationModel,omitempty"`
	ConversationEffort string `json:"conversationEffort,omitempty"`
	// PermissionMode is the chat's own permission mode (not the mode a
	// particular target had to run in).
	PermissionMode PermissionMode `json:"permissionMode,omitempty"`
	// FastMode, FastModeOff, Ultracode and AutoCompactWindow are the
	// launch choices the session was started with.
	FastMode          bool   `json:"fastMode,omitempty"`
	FastModeOff       bool   `json:"fastModeOff,omitempty"`
	Ultracode         bool   `json:"ultracode,omitempty"`
	AutoCompactWindow string `json:"autoCompactWindow,omitempty"`
}

// TemporaryEscalation is one manual temporary model escalation for one
// session. At most one non-terminal escalation exists per session.
type TemporaryEscalation struct {
	ID        string    `json:"id"`
	SessionID SessionID `json:"sessionId"`
	// ConversationID is the AO conversation the worker runs in.
	ConversationID string `json:"conversationId"`
	// WorkItemID binds the escalation to the current task: the latest
	// conversation turn at escalation time. Empty when the session had no
	// turn yet; then any completion after creation belongs to its first
	// task. A completion for another turn never triggers this revert.
	WorkItemID string `json:"workItemId,omitempty"`
	// GenerationBefore is the controller fence at escalation time
	// (ControllerGeneration for Chat, RuntimeLaunchID for TUI).
	// GenerationAfter is the fence after the upgrade switch committed.
	// A completion naming neither fence is stale and is ignored.
	GenerationBefore string `json:"generationBefore,omitempty"`
	GenerationAfter  string `json:"generationAfter,omitempty"`
	// Mode is the session mode at escalation time (chat or tui). The
	// revert uses the same mode's switch path.
	Mode SessionMode `json:"mode,omitempty"`

	Original ModelSnapshot `json:"original"`

	TargetHarness           AgentHarness   `json:"targetHarness"`
	TargetModel             string         `json:"targetModel,omitempty"`
	TargetEffort            string         `json:"targetEffort,omitempty"`
	TargetFastMode          bool           `json:"targetFastMode,omitempty"`
	TargetUltracode         bool           `json:"targetUltracode,omitempty"`
	TargetAutoCompactWindow string         `json:"targetAutoCompactWindow,omitempty"`
	TargetPermissionMode    PermissionMode `json:"targetPermissionMode,omitempty"`

	State        TemporaryEscalationState `json:"state"`
	DoneReportID string                   `json:"doneReportId,omitempty"`
	DoneAt       time.Time                `json:"doneAt,omitempty"`
	// AttemptCount counts revert attempts (not the upgrade).
	AttemptCount int    `json:"attemptCount"`
	LastError    string `json:"lastError,omitempty"`

	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	RevertedAt *time.Time `json:"revertedAt,omitempty"`
}

// ErrInvalidEscalation reports an invalid escalation record or transition.
var ErrInvalidEscalation = errors.New("invalid temporary escalation")

// Validate checks the record's identity, snapshot and state invariants.
// ConversationID may be empty for sessions without a chat conversation
// (TUI sessions); those escalations bind to the session and its fence
// instead of a turn.
func (e TemporaryEscalation) Validate() error {
	if e.ID == "" || e.SessionID == "" {
		return ErrInvalidEscalation
	}
	if e.Original.Harness == "" || e.TargetHarness == "" {
		return ErrInvalidEscalation
	}
	if !e.State.Valid() {
		return ErrInvalidEscalation
	}
	if e.CreatedAt.IsZero() || e.UpdatedAt.IsZero() {
		return ErrInvalidEscalation
	}
	if e.AttemptCount < 0 {
		return ErrInvalidEscalation
	}
	if e.State == TemporaryEscalationReverted && e.RevertedAt == nil {
		return ErrInvalidEscalation
	}
	if e.State == TemporaryEscalationAwaitingIdle && (e.DoneReportID == "" || e.DoneAt.IsZero()) {
		return ErrInvalidEscalation
	}
	return nil
}

// ErrEscalationTransition reports a state transition the current state
// does not allow.
var ErrEscalationTransition = errors.New("temporary escalation state transition not allowed")

func (e *TemporaryEscalation) touch(now time.Time) {
	e.UpdatedAt = now
}

// MarkDone records an accepted, correlated completion and moves a pending
// escalation to awaiting idle. A duplicate done for the same report is a
// no-op; anything else is rejected so a second completion cannot re-arm
// a revert.
func (e *TemporaryEscalation) MarkDone(reportID string, at time.Time) error {
	if reportID == "" || at.IsZero() {
		return ErrInvalidEscalation
	}
	if e.DoneReportID == reportID {
		return nil
	}
	if e.State != TemporaryEscalationPending {
		return fmt.Errorf("%w: cannot record completion in state %q", ErrEscalationTransition, e.State)
	}
	e.DoneReportID = reportID
	e.DoneAt = at
	e.State = TemporaryEscalationAwaitingIdle
	e.touch(at)
	return nil
}

// BeginRevert moves a pending or awaiting-idle escalation to reverting.
// Only one caller can win the CAS on this transition, which is what makes
// the automatic restore happen exactly once.
func (e *TemporaryEscalation) BeginRevert(now time.Time) error {
	switch e.State {
	case TemporaryEscalationPending, TemporaryEscalationAwaitingIdle:
		e.State = TemporaryEscalationReverting
		e.AttemptCount++
		e.touch(now)
		return nil
	default:
		return fmt.Errorf("%w: cannot begin revert in state %q", ErrEscalationTransition, e.State)
	}
}

// MarkReverted records a successful restore. It only follows reverting,
// so a restore that already landed cannot be recorded twice.
func (e *TemporaryEscalation) MarkReverted(now time.Time) error {
	if e.State != TemporaryEscalationReverting {
		return fmt.Errorf("%w: cannot mark reverted in state %q", ErrEscalationTransition, e.State)
	}
	e.State = TemporaryEscalationReverted
	e.LastError = ""
	at := now
	e.RevertedAt = &at
	e.touch(now)
	return nil
}

// MarkFailed records a failed revert attempt. The worker and worktree are
// untouched by this transition; the escalation stays visible for an
// explicit retry or manual recovery.
func (e *TemporaryEscalation) MarkFailed(now time.Time, err error) error {
	if e.State != TemporaryEscalationReverting {
		return fmt.Errorf("%w: cannot mark failed in state %q", ErrEscalationTransition, e.State)
	}
	e.State = TemporaryEscalationFailed
	if err != nil {
		e.LastError = err.Error()
	}
	e.touch(now)
	return nil
}

// RetryRevert re-arms a failed escalation for one more revert attempt.
func (e *TemporaryEscalation) RetryRevert(now time.Time) error {
	if e.State != TemporaryEscalationFailed {
		return fmt.Errorf("%w: cannot retry revert in state %q", ErrEscalationTransition, e.State)
	}
	e.State = TemporaryEscalationReverting
	e.AttemptCount++
	e.touch(now)
	return nil
}

// MarkSuperseded records that the owner made a newer explicit model
// choice while this escalation was active. The old auto-revert intent is
// gone: AO must never bounce back to the snapshot afterwards.
func (e *TemporaryEscalation) MarkSuperseded(now time.Time) error {
	switch e.State {
	case TemporaryEscalationPending, TemporaryEscalationAwaitingIdle,
		TemporaryEscalationReverting, TemporaryEscalationFailed:
		e.State = TemporaryEscalationSuperseded
		e.touch(now)
		return nil
	default:
		return fmt.Errorf("%w: cannot supersede in state %q", ErrEscalationTransition, e.State)
	}
}

// MarkCancelled records that the owner cancelled the auto-restore while
// it was still waiting. A revert already in flight is left to finish.
func (e *TemporaryEscalation) MarkCancelled(now time.Time) error {
	switch e.State {
	case TemporaryEscalationPending, TemporaryEscalationAwaitingIdle:
		e.State = TemporaryEscalationCancelled
		e.touch(now)
		return nil
	default:
		return fmt.Errorf("%w: cannot cancel in state %q", ErrEscalationTransition, e.State)
	}
}

// MarkInterruptedByRestart moves a revert that was in flight when the
// daemon died to failed. Restart recovery never silently retries a
// half-finished switch: a human (or an explicit retry) decides.
func (e *TemporaryEscalation) MarkInterruptedByRestart(now time.Time) error {
	if e.State != TemporaryEscalationReverting {
		return fmt.Errorf("%w: cannot mark interrupted in state %q", ErrEscalationTransition, e.State)
	}
	e.State = TemporaryEscalationFailed
	e.LastError = "revert interrupted by daemon restart"
	e.touch(now)
	return nil
}

// UpgradeRequest is the provider switch that performs the escalation.
func (e TemporaryEscalation) UpgradeRequest() ProviderSwitchRequest {
	return ProviderSwitchRequest{
		TargetHarness:     e.TargetHarness,
		Model:             e.TargetModel,
		Effort:            e.TargetEffort,
		FastMode:          e.TargetFastMode,
		Ultracode:         e.TargetUltracode,
		AutoCompactWindow: e.TargetAutoCompactWindow,
		PermissionMode:    e.TargetPermissionMode,
	}
}

// RevertRequest is the provider switch that restores the snapshot.
func (e TemporaryEscalation) RevertRequest() ProviderSwitchRequest {
	return ProviderSwitchRequest{
		TargetHarness:     e.Original.Harness,
		Model:             e.Original.Model,
		Effort:            e.Original.Effort,
		FastMode:          e.Original.FastMode,
		Ultracode:         e.Original.Ultracode,
		AutoCompactWindow: e.Original.AutoCompactWindow,
		PermissionMode:    e.Original.PermissionMode,
	}
}

// RevertSettings are the conversation turn settings the revert restores
// on a same-harness Chat session.
func (e TemporaryEscalation) RevertSettings() ConversationSettings {
	return ConversationSettings{
		Model:             e.Original.ConversationModel,
		ReasoningEffort:   e.Original.ConversationEffort,
		ApprovalMode:      e.Original.PermissionMode,
		AutoCompactWindow: e.Original.AutoCompactWindow,
	}
}

// ManualSwitchObserver is notified of explicit owner model/provider
// changes. Implementations supersede a pending auto-restore so a newer
// explicit choice is authoritative; they must ignore the switches the
// escalation service performs itself.
type ManualSwitchObserver interface {
	NoteManualSwitch(ctx context.Context, id SessionID, req ProviderSwitchRequest) error
	NoteManualTurnSettings(ctx context.Context, id SessionID, settings ConversationSettings) error
}

// IsRevertRequest reports whether req is exactly the restore this
// escalation would perform. The manual-switch observer uses it to tell
// AO's own revert apart from an owner's newer explicit choice.
func (e TemporaryEscalation) IsRevertRequest(req ProviderSwitchRequest) bool {
	return req.TargetHarness == e.Original.Harness &&
		req.Model == e.Original.Model &&
		req.Effort == e.Original.Effort
}
