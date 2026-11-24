package workerquota

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestWorkerQuotaPriorityAndRecovery(t *testing.T) {
	now := time.Now()
	primary := Target{Harness: domain.HarnessCodex, Model: "primary", Effort: "high"}
	fallback := Target{Harness: domain.HarnessAgy, Model: "fallback"}
	blocked := Window{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), ResetAt: now.Add(time.Minute), Exhausted: true}
	stale := blocked
	stale.ValidUntil = now
	for _, tc := range []struct {
		name    string
		windows map[Target][]Window
		at      time.Time
		want    Target
	}{
		{"primary available", map[Target][]Window{primary: {{ObservedAt: now}}}, now, primary},
		{"unknown", nil, now, primary},
		{"fallback", map[Target][]Window{primary: {{}, blocked}}, now, fallback},
		{"both exhausted", map[Target][]Window{primary: {blocked}, fallback: {blocked}}, now, Target{}},
		{"primary recovered", map[Target][]Window{primary: {blocked}}, blocked.ResetAt, primary},
		{"stale", map[Target][]Window{primary: {stale}}, now, primary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Select([]Target{primary, fallback}, tc.windows, tc.at)
			if got != tc.want || ok != (tc.want != Target{}) {
				t.Fatalf("selected %#v, %v, want %#v", got, ok, tc.want)
			}
		})
	}
}

type quotaSourceFunc func(context.Context, []Target) (map[Target][]Window, error)

func (f quotaSourceFunc) Read(ctx context.Context, targets []Target) (map[Target][]Window, error) {
	return f(ctx, targets)
}

func TestWorkerQuotaNativeEvidenceUsesItsAccountAndReset(t *testing.T) {
	now := time.Now()
	target := Target{Harness: domain.HarnessCodex, Model: "a", Effort: "max"}
	policy := &Policy{Source: quotaSourceFunc(func(context.Context, []Target) (map[Target][]Window, error) {
		return nil, errors.New("unreachable")
	})}
	evidence := []Failure{{Target: Target{Harness: domain.HarnessCodex, Model: "a"}, At: now.Add(-time.Minute), ResetAt: now.Add(time.Minute), Scope: "computer login"}}
	windows, err := policy.Windows(context.Background(), []Target{target}, evidence, now)
	if err == nil {
		t.Fatal("source outage was hidden")
	}
	if _, ok := Select([]Target{target}, windows, now); ok {
		t.Fatal("native evidence was lost during outage")
	}
	policy.ComputerLogin = func(context.Context, domain.AgentHarness) bool { return false }
	windows, _ = policy.Windows(context.Background(), []Target{target}, evidence, now)
	if _, ok := Select([]Target{target}, windows, now); !ok {
		t.Fatal("computer login quota blocked a different account")
	}
}
