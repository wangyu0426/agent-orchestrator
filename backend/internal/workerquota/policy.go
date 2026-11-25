// Package workerquota holds the daemon-wide worker admission policy.
package workerquota

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Role identifies which AO execution role a quota policy governs.
type Role string

const (
	// RoleWorker governs automatic worker session launches and in-flight handoffs.
	RoleWorker Role = "worker"
	// RoleReviewer governs automatic PR reviewer invocations.
	RoleReviewer Role = "reviewer"
	// RoleOrchestrator governs automatic orchestrator launches and in-flight parking.
	RoleOrchestrator Role = "orchestrator"
)

// ErrDeferred indicates that quota blocked every otherwise eligible route.
var ErrDeferred = errors.New("automatic worker deferred: all eligible providers have exhausted quota")

// ErrNoReadyRoute indicates that no configured provider could be launched.
var ErrNoReadyRoute = errors.New("no configured worker quota route is ready to launch")

// DeferredError wraps ErrDeferred with role attribution and an optional
// reset-aware retry deadline computed from the exhausted quota windows.
type DeferredError struct {
	Role    Role
	RetryAt time.Time
	Reason  string
}

func (e *DeferredError) Error() string {
	role := e.Role
	if role == "" {
		role = RoleWorker
	}
	if e.Reason != "" {
		return "automatic " + string(role) + " deferred: " + e.Reason
	}
	return "automatic " + string(role) + " deferred: all eligible providers have exhausted quota"
}

func (e *DeferredError) Unwrap() error { return ErrDeferred }

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

// NextResetAt returns the earliest future reset (or validity expiry when reset
// is unknown) among currently exhausted windows for the given targets. It is
// used to compute a bounded, reset-aware retry deadline when all routes for a
// role are exhausted.
func NextResetAt(targets []Target, windows map[Target][]Window, now time.Time) (time.Time, bool) {
	var earliest time.Time
	for _, target := range targets {
		for _, w := range windows[target] {
			if !w.Exhausted || w.ObservedAt.IsZero() || w.ObservedAt.After(now) ||
				!now.Before(w.ValidUntil) || (!w.ResetAt.IsZero() && !now.Before(w.ResetAt)) {
				continue
			}
			candidate := w.ResetAt
			if candidate.IsZero() || w.ValidUntil.Before(candidate) {
				candidate = w.ValidUntil
			}
			if candidate.After(now) && (earliest.IsZero() || candidate.Before(earliest)) {
				earliest = candidate
			}
		}
	}
	return earliest, !earliest.IsZero()
}

// RolePolicies holds independent opt-in quota policies for Worker, Reviewer,
// and Orchestrator. All enabled roles share the same Quotum source and native
// failure evidence.
type RolePolicies struct {
	Worker       *Policy
	Reviewer     *Policy
	Orchestrator *Policy
}

// AnyEnabled reports whether at least one role has quota failover configured.
func (r RolePolicies) AnyEnabled() bool {
	return r.Worker != nil || r.Reviewer != nil || r.Orchestrator != nil
}

// ForRole returns the policy for role, or nil when that role is not opted in.
func (r RolePolicies) ForRole(role Role) *Policy {
	switch role {
	case RoleWorker:
		return r.Worker
	case RoleReviewer:
		return r.Reviewer
	case RoleOrchestrator:
		return r.Orchestrator
	default:
		return nil
	}
}

// Policy applies one ordered configuration across projects for a single role.
type Policy struct {
	Role                 Role
	Primary              *Target
	Fallbacks            []Target
	Source               Source
	ComputerLogin        func(context.Context, domain.AgentHarness) bool
	DiscoverDefaultModel func(context.Context, domain.ProjectRecord, domain.AgentHarness) string
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

// DefaultModels resolves each omitted model at most once per decision.
func (p *Policy) DefaultModels(ctx context.Context, project domain.ProjectRecord, targets []Target) map[domain.AgentHarness]string {
	if p.DiscoverDefaultModel == nil {
		return nil
	}
	defaults := make(map[domain.AgentHarness]string)
	for _, target := range targets {
		if _, seen := defaults[target.Harness]; target.Model == "" && !seen {
			defaults[target.Harness] = p.DiscoverDefaultModel(ctx, project, target.Harness)
		}
	}
	return defaults
}

// Windows reads the optional Quotum snapshot and durable native reset evidence.
// Quotum bindings describe the computer login only. Saved accounts (non-empty
// Target.Account) may use the same model name while consuming a different quota.
func (p *Policy) Windows(ctx context.Context, targets []Target, failures []Failure, now time.Time, defaults map[domain.AgentHarness]string) (map[Target][]Window, error) {
	computerBound := make([]Target, 0, len(targets))
	for _, target := range targets {
		if target.Account == "" && (p.ComputerLogin == nil || p.ComputerLogin(ctx, target.Harness)) {
			computerBound = append(computerBound, target)
		}
	}
	windows := make(map[Target][]Window, len(targets))
	var err error
	if p.Source != nil && len(computerBound) != 0 {
		snapshot, readErr := p.Source.Read(ctx, computerBound)
		err = readErr
		if readErr == nil {
			for _, target := range computerBound {
				windows[target] = snapshot[target]
			}
		}
	}
	for _, target := range targets {
		if target.Account == "" && p.ComputerLogin != nil && !p.ComputerLogin(ctx, target.Harness) {
			continue
		}
		model := target.Model
		if model == "" {
			model = defaults[target.Harness]
		}
		for _, f := range failures {
			scopeMatches := (target.Account == "" && f.Scope != "" && f.Scope == "computer login") ||
				(target.Account != "" && f.Scope == target.Account)
			modelMatches := (f.Target.Model == "" && f.Target.Harness == target.Harness) ||
				(model != "" && f.Target.Model == model && f.Target.Harness == target.Harness)
			if scopeMatches && modelMatches && f.ResetAt.After(now) {
				windows[target] = append(windows[target], Window{ObservedAt: f.At, ValidUntil: f.ResetAt, ResetAt: f.ResetAt, Exhausted: true})
			}
		}
	}
	return windows, err
}
