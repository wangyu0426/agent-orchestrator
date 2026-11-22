package domain

import (
	"errors"
	"testing"
	"time"
)

func testEscalation(now time.Time) TemporaryEscalation {
	return TemporaryEscalation{
		ID:             "esc_1",
		SessionID:      "sess-1",
		ConversationID: "conv-1",
		WorkItemID:     "turn-1",
		Original: ModelSnapshot{
			Harness:           HarnessCodex,
			Model:             "mai-code-2",
			Effort:            "medium",
			ConversationModel: "mai-code-2",
			PermissionMode:    PermissionModeAuto,
		},
		TargetHarness: HarnessClaudeCode,
		TargetModel:   "sol-6.2",
		State:         TemporaryEscalationPending,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func TestTemporaryEscalationValidate(t *testing.T) {
	now := time.Now()
	valid := testEscalation(now)
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	for name, mutate := range map[string]func(*TemporaryEscalation){
		"missing id":        func(e *TemporaryEscalation) { e.ID = "" },
		"missing session":   func(e *TemporaryEscalation) { e.SessionID = "" },
		"missing original":  func(e *TemporaryEscalation) { e.Original.Harness = "" },
		"missing target":    func(e *TemporaryEscalation) { e.TargetHarness = "" },
		"bad state":         func(e *TemporaryEscalation) { e.State = "nope" },
		"reverted no time":  func(e *TemporaryEscalation) { e.State = TemporaryEscalationReverted },
		"awaiting no done":  func(e *TemporaryEscalation) { e.State = TemporaryEscalationAwaitingIdle },
		"negative attempts": func(e *TemporaryEscalation) { e.AttemptCount = -1 },
	} {
		e := testEscalation(now)
		mutate(&e)
		if err := e.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error", name)
		}
	}
}

func TestTemporaryEscalationStateHelpers(t *testing.T) {
	if !TemporaryEscalationPending.Active() || !TemporaryEscalationAwaitingIdle.Active() || !TemporaryEscalationReverting.Active() {
		t.Error("pending/awaiting_idle/reverting should be Active")
	}
	if TemporaryEscalationFailed.Active() || TemporaryEscalationReverted.Active() {
		t.Error("failed/reverted should not be Active")
	}
	if !TemporaryEscalationReverted.Terminal() || !TemporaryEscalationSuperseded.Terminal() || !TemporaryEscalationCancelled.Terminal() {
		t.Error("reverted/superseded/cancelled should be Terminal")
	}
	if TemporaryEscalationFailed.Terminal() {
		t.Error("failed must not be Terminal: it waits for an explicit retry")
	}
	if TemporaryEscalationState("nope").Valid() {
		t.Error("unknown state should not be Valid")
	}
}

func TestTemporaryEscalationHappyPath(t *testing.T) {
	now := time.Now()
	e := testEscalation(now)
	doneAt := now.Add(time.Minute)
	if err := e.MarkDone("rpt_1", doneAt); err != nil {
		t.Fatalf("MarkDone() = %v", err)
	}
	if e.State != TemporaryEscalationAwaitingIdle || e.DoneReportID != "rpt_1" {
		t.Fatalf("after MarkDone state = %q done = %q", e.State, e.DoneReportID)
	}
	// A duplicate done for the same report is a no-op, not an error.
	if err := e.MarkDone("rpt_1", doneAt.Add(time.Minute)); err != nil {
		t.Fatalf("duplicate MarkDone() = %v, want nil", err)
	}
	if err := e.BeginRevert(doneAt); err != nil {
		t.Fatalf("BeginRevert() = %v", err)
	}
	if e.AttemptCount != 1 {
		t.Fatalf("AttemptCount = %d, want 1", e.AttemptCount)
	}
	revertedAt := doneAt.Add(2 * time.Minute)
	if err := e.MarkReverted(revertedAt); err != nil {
		t.Fatalf("MarkReverted() = %v", err)
	}
	if e.State != TemporaryEscalationReverted || e.RevertedAt == nil || !e.RevertedAt.Equal(revertedAt) {
		t.Fatalf("after MarkReverted state = %q revertedAt = %v", e.State, e.RevertedAt)
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("Validate() after revert = %v", err)
	}
	// Exactly once: no second revert, no second completion.
	if err := e.BeginRevert(revertedAt); err == nil {
		t.Error("BeginRevert() after reverted = nil, want error")
	}
	if err := e.MarkDone("rpt_2", revertedAt); err == nil {
		t.Error("MarkDone() after reverted = nil, want error")
	}
}

func TestTemporaryEscalationFailureAndRetry(t *testing.T) {
	now := time.Now()
	e := testEscalation(now)
	if err := e.BeginRevert(now); err != nil {
		t.Fatalf("BeginRevert() = %v", err)
	}
	if err := e.MarkFailed(now, errors.New("target provider failed to start")); err != nil {
		t.Fatalf("MarkFailed() = %v", err)
	}
	if e.State != TemporaryEscalationFailed || e.LastError == "" {
		t.Fatalf("after MarkFailed state = %q lastError = %q", e.State, e.LastError)
	}
	// Failed never reverts on its own: BeginRevert is refused.
	if err := e.BeginRevert(now); err == nil {
		t.Error("BeginRevert() from failed = nil, want error")
	}
	if err := e.RetryRevert(now); err != nil {
		t.Fatalf("RetryRevert() = %v", err)
	}
	if e.State != TemporaryEscalationReverting || e.AttemptCount != 2 {
		t.Fatalf("after RetryRevert state = %q attempts = %d", e.State, e.AttemptCount)
	}
	if err := e.MarkReverted(now); err != nil {
		t.Fatalf("MarkReverted() after retry = %v", err)
	}
}

func TestTemporaryEscalationSupersedeAndCancel(t *testing.T) {
	now := time.Now()
	e := testEscalation(now)
	if err := e.MarkSuperseded(now); err != nil {
		t.Fatalf("MarkSuperseded() from pending = %v", err)
	}
	if e.State != TemporaryEscalationSuperseded {
		t.Fatalf("state = %q, want superseded", e.State)
	}
	if err := e.MarkSuperseded(now); err == nil {
		t.Error("second MarkSuperseded() = nil, want error")
	}

	e = testEscalation(now)
	if err := e.MarkCancelled(now); err != nil {
		t.Fatalf("MarkCancelled() from pending = %v", err)
	}
	if err := e.MarkCancelled(now); err == nil {
		t.Error("second MarkCancelled() = nil, want error")
	}

	// A revert interrupted by a daemon restart becomes failed, not
	// silently retried and not reverted.
	e = testEscalation(now)
	if err := e.BeginRevert(now); err != nil {
		t.Fatalf("BeginRevert() = %v", err)
	}
	if err := e.MarkInterruptedByRestart(now); err != nil {
		t.Fatalf("MarkInterruptedByRestart() = %v", err)
	}
	if e.State != TemporaryEscalationFailed {
		t.Fatalf("state = %q, want failed", e.State)
	}
}

func TestTemporaryEscalationRequests(t *testing.T) {
	e := testEscalation(time.Now())
	revert := e.RevertRequest()
	if revert.TargetHarness != HarnessCodex || revert.Model != "mai-code-2" || revert.Effort != "medium" {
		t.Fatalf("RevertRequest() = %+v, want original codex/mai-code-2/medium", revert)
	}
	upgrade := e.UpgradeRequest()
	if upgrade.TargetHarness != HarnessClaudeCode || upgrade.Model != "sol-6.2" {
		t.Fatalf("UpgradeRequest() = %+v, want claude-code/sol-6.2", upgrade)
	}
	if !e.IsRevertRequest(revert) {
		t.Error("IsRevertRequest(revert) = false, want true")
	}
	if e.IsRevertRequest(upgrade) {
		t.Error("IsRevertRequest(upgrade) = true, want false")
	}
	settings := e.RevertSettings()
	if settings.Model != "mai-code-2" || settings.ReasoningEffort != "" {
		t.Fatalf("RevertSettings() = %+v", settings)
	}
}
