// Package workerquota holds the daemon-wide worker admission policy.
package workerquota

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrDeferred indicates that quota blocked every otherwise eligible route.
var ErrDeferred = errors.New("automatic worker deferred: all eligible providers have exhausted quota")

// Target describes a worker launch choice.
type Target = domain.WorkerQuotaTarget

// Window is one provider limit for the account and pool associated with a target.
type Window struct {
	ObservedAt time.Time
	ValidUntil time.Time
	ResetAt    time.Time
	Exhausted  bool
}

// Source returns known applicable quota windows. An absent or unreadable
// reading is unknown. Policies must never manufacture an exhausted window.
type Source interface {
	Read(context.Context, []Target) (map[Target][]Window, error)
}

// Select chooses the first target with no applicable, currently exhausted
// window. Unknown quota is eligible. A reset expires the old evidence.
func Select(targets []Target, windows map[Target][]Window, now time.Time) (Target, bool) {
	for _, target := range targets {
		exhausted := false
		for _, w := range windows[target] {
			if w.Exhausted && !w.ObservedAt.IsZero() && !w.ObservedAt.After(now) &&
				now.Before(w.ValidUntil) && (w.ResetAt.IsZero() || now.Before(w.ResetAt)) {
				exhausted = true
				break
			}
		}
		if !exhausted {
			return target, true
		}
	}
	return Target{}, false
}

// Policy applies one ordered worker configuration across projects.
type Policy struct {
	Primary       *Target
	Fallbacks     []Target
	Source        Source
	ComputerLogin func(context.Context, domain.AgentHarness) bool
}

// Targets preserves the first occurrence of each launch choice.
func (p *Policy) Targets(project Target) []Target {
	if p.Primary != nil {
		project = *p.Primary
	}
	targets := make([]Target, 0, len(p.Fallbacks)+1)
	seen := make(map[Target]bool, cap(targets))
	for _, target := range append([]Target{project}, p.Fallbacks...) {
		if target.Harness != "" && !seen[target] {
			seen[target] = true
			targets = append(targets, target)
		}
	}
	return targets
}

// Session is the durable recovery budget and diagnostics for one worker.
type Session struct {
	ID        domain.SessionID
	Initial   Target
	Remaining []Target
	Target    Target
	Attempt   string
	Reason    string
	FailureAt time.Time
}

// Failure is authoritative native evidence for one launch route.
type Failure struct {
	Target  Target
	At      time.Time
	ResetAt time.Time
	Scope   string // durable AO account scope; empty disables new-worker admission evidence
}

// Windows reads the optional Quotum snapshot and durable native reset evidence.
// Quotum bindings describe the computer login only. Saved accounts may use the
// same model name while consuming a different quota.
func (p *Policy) Windows(ctx context.Context, targets []Target, failures []Failure, now time.Time) (map[Target][]Window, error) {
	bound := make([]Target, 0, len(targets))
	for _, target := range targets {
		if p.ComputerLogin == nil || p.ComputerLogin(ctx, target.Harness) {
			bound = append(bound, target)
		}
	}
	windows := make(map[Target][]Window, len(bound))
	var err error
	if p.Source != nil && len(bound) != 0 {
		snapshot, readErr := p.Source.Read(ctx, bound)
		err = readErr
		if readErr == nil {
			for _, target := range bound {
				windows[target] = snapshot[target]
			}
		}
	}
	for _, target := range bound {
		for _, f := range failures {
			if f.Scope != "" && f.Target.Harness == target.Harness && f.Target.Model == target.Model && f.ResetAt.After(now) {
				windows[target] = append(windows[target], Window{ObservedAt: f.At, ValidUntil: f.ResetAt, ResetAt: f.ResetAt, Exhausted: true})
			}
		}
	}
	return windows, err
}
