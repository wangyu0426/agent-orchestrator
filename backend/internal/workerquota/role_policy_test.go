package workerquota

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type staticSource map[Target][]Window

func (s staticSource) Read(_ context.Context, targets []Target) (map[Target][]Window, error) {
	out := make(map[Target][]Window, len(targets))
	for _, t := range targets {
		out[t] = s[t]
	}
	return out, nil
}

func TestNextResetAtPicksEarliestActiveExhaustedWindow(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	codex := Target{Harness: domain.HarnessCodex, Model: "codex-max"}
	agy := Target{Harness: domain.HarnessAgy, Model: "agy-pro"}
	windows := map[Target][]Window{
		codex: {
			// Already expired reset: ignored.
			{ObservedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour), ResetAt: now.Add(-time.Minute), Exhausted: true},
			// Active exhausted window resetting in 3 hours.
			{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(4 * time.Hour), ResetAt: now.Add(3 * time.Hour), Exhausted: true},
		},
		agy: {
			// Not exhausted: ignored.
			{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(30 * time.Minute), ResetAt: now.Add(10 * time.Minute), Exhausted: false},
			// Exhausted with unknown reset: falls back to ValidUntil (45m).
			{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(45 * time.Minute), Exhausted: true},
		},
	}
	got, ok := NextResetAt([]Target{codex, agy}, windows, now)
	if !ok {
		t.Fatal("NextResetAt = not ok, want true")
	}
	want := now.Add(45 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("NextResetAt = %v, want %v", got, want)
	}
}

func TestRepeatedHarnessWithDistinctAccountsIsEligible(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	computerCodex := Target{Harness: domain.HarnessCodex, Model: "codex-max"}
	savedCodex := Target{Harness: domain.HarnessCodex, Model: "codex-max", Account: "acct-work"}
	policy := &Policy{
		Role:      RoleOrchestrator,
		Primary:   &computerCodex,
		Fallbacks: []Target{savedCodex},
		Source: staticSource{
			computerCodex: {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), ResetAt: now.Add(2 * time.Hour), Exhausted: true}},
		},
	}
	targets := policy.Targets(computerCodex)
	if len(targets) != 2 {
		t.Fatalf("Targets = %v, want 2 distinct targets", targets)
	}
	windows, err := policy.Windows(context.Background(), targets, nil, now, nil)
	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	selected, ok := Select(targets, windows, now)
	if !ok || selected != savedCodex {
		t.Fatalf("Select = (%+v, %v), want (%+v, true)", selected, ok, savedCodex)
	}
}

func TestDeferredErrorUnwrapsErrDeferred(t *testing.T) {
	err := &DeferredError{Role: RoleOrchestrator, RetryAt: time.Now().Add(time.Minute)}
	if !errors.Is(err, ErrDeferred) {
		t.Fatalf("errors.Is(%v, ErrDeferred) = false, want true", err)
	}
}

func TestRolePoliciesLookup(t *testing.T) {
	w := &Policy{Role: RoleWorker}
	r := &Policy{Role: RoleReviewer}
	o := &Policy{Role: RoleOrchestrator}
	rp := RolePolicies{Worker: w, Reviewer: r, Orchestrator: o}
	if !rp.AnyEnabled() {
		t.Fatal("AnyEnabled = false, want true")
	}
	if rp.ForRole(RoleWorker) != w || rp.ForRole(RoleReviewer) != r || rp.ForRole(RoleOrchestrator) != o {
		t.Fatal("ForRole did not return expected role policies")
	}
}
