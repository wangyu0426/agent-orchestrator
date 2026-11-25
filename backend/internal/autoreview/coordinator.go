// Package autoreview decides when the daemon may automatically review a
// worker's current pull-request heads.
package autoreview

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	reviewcore "github.com/aoagents/agent-orchestrator/backend/internal/review"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

const (
	// DefaultIdleThreshold is how long a worker must remain idle before an
	// automatic review may start.
	DefaultIdleThreshold = time.Minute
	// DefaultSweepInterval is the cadence for reevaluating live sessions.
	DefaultSweepInterval = time.Minute
	// autoReviewFailedRetryLimit bounds retries for the same current PR head.
	autoReviewFailedRetryLimit = domain.AutoReviewFailedRetryLimit
)

// Store provides the durable session, project, PR, and review facts used by
// the coordinator.
type Store interface {
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	ListAllSessions(context.Context) ([]domain.SessionRecord, error)
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
	ListPRsBySession(context.Context, domain.SessionID) ([]domain.PullRequest, error)
	ListReviewRunsBySession(context.Context, domain.SessionID) ([]domain.ReviewRun, error)
}

// Trigger starts a source-tagged automatic review through the normal review
// engine.
type Trigger interface {
	TriggerAuto(context.Context, domain.SessionID, domain.ReviewerHarness) (reviewcore.TriggerResult, error)
}

// DeferralStore is an optional Store extension that persists reset-aware
// reviewer quota deferrals keyed by (session, PR, HEAD) across daemon restarts.
type DeferralStore interface {
	UpsertReviewerQuotaDeferral(ctx context.Context, sessionID domain.SessionID, prURL, targetSHA string, retryAt time.Time, reason string, updatedAt time.Time) error
	GetReviewerQuotaDeferral(ctx context.Context, sessionID domain.SessionID, prURL, targetSHA string) (retryAt time.Time, reason string, found bool, err error)
	DeleteReviewerQuotaDeferral(ctx context.Context, sessionID domain.SessionID, prURL, targetSHA string) error
}

// OptionsTrigger is an optional Trigger extension that accepts full
// TriggerOptions (model, effort, interface mode) for quota-selected routes.
type OptionsTrigger interface {
	TriggerAutoWithOptions(context.Context, domain.SessionID, reviewcore.TriggerOptions) (reviewcore.TriggerResult, error)
}

// Result describes whether an evaluation started a review and why it skipped
// or triggered.
type Result struct {
	Triggered bool
	Reason    string
	RetryAt   time.Time
}

// Coordinator evaluates auto-review policy and owns the periodic sweep.
type Coordinator struct {
	store         Store
	reviews       Trigger
	clock         func() time.Time
	idleThreshold time.Duration
	sweepInterval time.Duration
	logger        *slog.Logger
	quota         *workerquota.Policy
	quotaFailures func(context.Context, time.Time) ([]workerquota.Failure, error)
	routeReady    func(context.Context, workerquota.Target) bool

	deferMu       sync.Mutex
	deferredUntil map[domain.SessionID]time.Time
}

// Config customizes coordinator timing, logging, and optional reviewer quota failover.
type Config struct {
	Clock         func() time.Time
	IdleThreshold time.Duration
	SweepInterval time.Duration
	Logger        *slog.Logger
	Quota         *workerquota.Policy
	QuotaFailures func(context.Context, time.Time) ([]workerquota.Failure, error)
	RouteReady    func(context.Context, workerquota.Target) bool
}

// New constructs an auto-review coordinator.
func New(store Store, reviews Trigger, cfg Config) *Coordinator {
	c := &Coordinator{
		store:         store,
		reviews:       reviews,
		clock:         cfg.Clock,
		idleThreshold: cfg.IdleThreshold,
		sweepInterval: cfg.SweepInterval,
		logger:        cfg.Logger,
		quota:         cfg.Quota,
		quotaFailures: cfg.QuotaFailures,
		routeReady:    cfg.RouteReady,
		deferredUntil: make(map[domain.SessionID]time.Time),
	}
	if c.clock == nil {
		c.clock = time.Now
	}
	if c.idleThreshold <= 0 {
		c.idleThreshold = DefaultIdleThreshold
	}
	if c.sweepInterval <= 0 {
		c.sweepInterval = DefaultSweepInterval
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	return c
}

// EvaluateSession evaluates one worker against the current project, activity,
// PR, and review-run facts.
func (c *Coordinator) EvaluateSession(ctx context.Context, id domain.SessionID) (Result, error) {
	session, ok, err := c.store.GetSession(ctx, id)
	if err != nil || !ok {
		if err == nil {
			return Result{Reason: "session_not_found"}, nil
		}
		return Result{}, err
	}
	if reason := sessionGate(session, c.clock(), c.idleThreshold); reason != "" {
		return Result{Reason: reason}, nil
	}
	project, ok, err := c.store.GetProject(ctx, string(session.ProjectID))
	if err != nil || !ok {
		if err == nil {
			return Result{Reason: "missing_reviewer_harness"}, nil
		}
		return Result{}, err
	}
	now := c.clock()
	prs, err := c.store.ListPRsBySession(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if len(prs) == 0 {
		return Result{Reason: "no_pr"}, nil
	}
	runs, err := c.store.ListReviewRunsBySession(ctx, id)
	if err != nil {
		return Result{}, err
	}
	harness := effectiveReviewerHarness(session, project.Config)
	var selectedRoute *workerquota.Target
	if c.quota != nil && session.ReviewerHarness == "" {
		// Reconcile persisted per-(session, PR, HEAD) deferrals: a new HEAD
		// bypasses an older deferral automatically.
		if retryAt, deferred := c.activeDeferral(ctx, id, prs, now); deferred {
			return Result{Reason: "reviewer_quota_deferred", RetryAt: retryAt}, nil
		}
		route, retryAt, quotaReason := c.selectReviewerRoute(ctx, project, harness, now)
		if quotaReason != "" {
			if quotaReason == "reviewer_quota_deferred" {
				c.persistDeferrals(ctx, id, prs, retryAt, quotaReason, now)
				c.logger.Info("auto-review: all reviewer quota routes exhausted; deferring",
					"role", workerquota.RoleReviewer, "session_id", id, "retry_at", retryAt)
				return Result{Reason: quotaReason, RetryAt: retryAt}, nil
			}
			return Result{Reason: quotaReason}, nil
		}
		if route.Harness != "" {
			selectedRoute = &route
			harness = domain.ReviewerHarness(route.Harness)
		}
	}
	if harness == "" {
		return Result{Reason: "missing_reviewer_harness"}, nil
	}
	harnessRuns := make([]domain.ReviewRun, 0, len(runs))
	for _, run := range runs {
		if run.Harness == harness || run.Harness == "" {
			harnessRuns = append(harnessRuns, run)
		}
	}
	states := reviewcore.Plan(prs, harnessRuns)
	if len(states) == 0 {
		return Result{Reason: "no_pr"}, nil
	}
	eligible := false
	hasRunning := false
	reason := "planner_ineligible"
	for _, state := range states {
		switch state.Status {
		case reviewcore.ReviewStateRunning:
			hasRunning = true
			reason = "review_running"
		case reviewcore.ReviewStateUpToDate:
			reason = "already_approved"
		case reviewcore.ReviewStateChangesRequested:
			reason = "changes_requested_same_sha"
		case reviewcore.ReviewStateIneligible:
			reason = ineligibleReason(prs, state.PRURL)
		case reviewcore.ReviewStateNeedsReview:
			if blocked := existingHeadReason(harnessRuns, state.PRURL, state.TargetSHA); blocked != "" {
				reason = blocked
				continue
			}
			// When quota failover selects a different reviewer harness, never
			// duplicate a review that is already running or decided on this HEAD.
			if selectedRoute != nil {
				if crossBlocked := crossHarnessHeadReason(runs, state.PRURL, state.TargetSHA); crossBlocked != "" {
					reason = crossBlocked
					continue
				}
			}
			eligible = true
		}
	}
	if !eligible && !hasRunning {
		return Result{Reason: reason}, nil
	}
	var result reviewcore.TriggerResult
	if selectedRoute != nil {
		if optTrigger, ok := c.reviews.(OptionsTrigger); ok {
			result, err = optTrigger.TriggerAutoWithOptions(ctx, id, reviewcore.TriggerOptions{
				Harness:                    harness,
				Config:                     domain.AgentConfig{Model: selectedRoute.Model, Effort: selectedRoute.Effort},
				InterfaceMode:              domain.ReviewerInterfaceMode(selectedRoute.Mode),
				Source:                     domain.ReviewTriggerAuto,
				DeduplicateAcrossHarnesses: true,
			})
		} else {
			result, err = c.reviews.TriggerAuto(ctx, id, harness)
		}
	} else {
		result, err = c.reviews.TriggerAuto(ctx, id, harness)
	}
	if err != nil {
		return Result{}, fmt.Errorf("trigger auto review for %s: %w", id, err)
	}
	if result.SkipReason != "" {
		return Result{Reason: result.SkipReason}, nil
	}
	if !result.Created {
		return Result{Reason: triggerResultReason(prs, result, harness)}, nil
	}
	c.clearDeferrals(ctx, id, prs)
	return Result{Triggered: true, Reason: "triggered"}, nil
}

func (c *Coordinator) activeDeferral(ctx context.Context, id domain.SessionID, prs []domain.PullRequest, now time.Time) (time.Time, bool) {
	if ds, ok := c.store.(DeferralStore); ok {
		var earliest time.Time
		allDeferred := len(prs) > 0
		for _, pr := range prs {
			if pr.URL == "" || pr.HeadSHA == "" || pr.Draft || pr.Merged || pr.Closed {
				continue
			}
			retryAt, _, found, err := ds.GetReviewerQuotaDeferral(ctx, id, pr.URL, pr.HeadSHA)
			if err != nil || !found || !now.Before(retryAt) {
				if found && !now.Before(retryAt) {
					_ = ds.DeleteReviewerQuotaDeferral(ctx, id, pr.URL, pr.HeadSHA)
				}
				allDeferred = false
				continue
			}
			if earliest.IsZero() || retryAt.Before(earliest) {
				earliest = retryAt
			}
		}
		if allDeferred && !earliest.IsZero() {
			return earliest, true
		}
		return time.Time{}, false
	}
	c.deferMu.Lock()
	defer c.deferMu.Unlock()
	until, ok := c.deferredUntil[id]
	if !ok {
		return time.Time{}, false
	}
	if !now.Before(until) {
		delete(c.deferredUntil, id)
		return time.Time{}, false
	}
	return until, true
}

func (c *Coordinator) persistDeferrals(ctx context.Context, id domain.SessionID, prs []domain.PullRequest, retryAt time.Time, reason string, now time.Time) {
	if ds, ok := c.store.(DeferralStore); ok {
		for _, pr := range prs {
			if pr.URL == "" || pr.HeadSHA == "" || pr.Draft || pr.Merged || pr.Closed {
				continue
			}
			if err := ds.UpsertReviewerQuotaDeferral(ctx, id, pr.URL, pr.HeadSHA, retryAt, reason, now); err != nil {
				c.logger.Warn("auto-review: persist reviewer quota deferral failed", "session_id", id, "pr_url", pr.URL, "err", err)
			}
		}
		return
	}
	c.deferMu.Lock()
	defer c.deferMu.Unlock()
	c.deferredUntil[id] = retryAt
}

func (c *Coordinator) clearDeferrals(ctx context.Context, id domain.SessionID, prs []domain.PullRequest) {
	if ds, ok := c.store.(DeferralStore); ok {
		for _, pr := range prs {
			if pr.URL != "" && pr.HeadSHA != "" {
				_ = ds.DeleteReviewerQuotaDeferral(ctx, id, pr.URL, pr.HeadSHA)
			}
		}
		return
	}
	c.deferMu.Lock()
	defer c.deferMu.Unlock()
	delete(c.deferredUntil, id)
}

func (c *Coordinator) selectReviewerRoute(ctx context.Context, project domain.ProjectRecord, projectHarness domain.ReviewerHarness, now time.Time) (workerquota.Target, time.Time, string) {
	var primaryModel, primaryEffort string
	if len(project.Config.Reviewers) > 0 && project.Config.Reviewers[0].Harness == projectHarness {
		primaryModel = project.Config.Reviewers[0].AgentConfig.Model
		primaryEffort = project.Config.Reviewers[0].AgentConfig.Effort
	} else if h, cfg := project.Config.DefaultWorkerReviewer(); h == projectHarness {
		primaryModel = cfg.Model
		primaryEffort = cfg.Effort
	}
	primary := workerquota.Target{
		Harness: domain.AgentHarness(projectHarness),
		Model:   primaryModel,
		Effort:  primaryEffort,
	}
	targets := c.quota.Targets(primary)
	if len(targets) == 0 {
		return workerquota.Target{}, time.Time{}, ""
	}
	defaults := c.quota.DefaultModels(ctx, project, targets)
	var failures []workerquota.Failure
	if c.quotaFailures != nil {
		var err error
		failures, err = c.quotaFailures(ctx, now)
		if err != nil {
			c.logger.Warn("auto-review: quota failure records unavailable", "error", err)
		}
	}
	windows, err := c.quota.Windows(ctx, targets, failures, now, defaults)
	if err != nil {
		c.logger.Debug("auto-review: quota source unavailable", "error", err)
	}
	blocked := false
	for _, route := range targets {
		if _, ok := workerquota.Select([]workerquota.Target{route}, windows, now); !ok {
			blocked = true
			continue
		}
		if c.routeReady != nil && !c.routeReady(ctx, route) {
			continue
		}
		if route != primary || route.Mode != "" || route.Account != "" {
			c.logger.Info("auto-review: reviewer quota selected configured route",
				"role", workerquota.RoleReviewer, "project", project.ID,
				"harness", route.Harness, "model", route.Model, "effort", route.Effort,
				"mode", route.Mode, "account", route.Account)
		}
		return route, time.Time{}, ""
	}
	if blocked {
		retryAt, ok := workerquota.NextResetAt(targets, windows, now)
		if !ok {
			retryAt = now.Add(c.sweepInterval)
		}
		return workerquota.Target{}, retryAt, "reviewer_quota_deferred"
	}
	return workerquota.Target{}, time.Time{}, "reviewer_quota_no_ready_route"
}

func crossHarnessHeadReason(runs []domain.ReviewRun, prURL, targetSHA string) string {
	for _, run := range runs {
		if run.PRURL != prURL || run.TargetSHA != targetSHA {
			continue
		}
		if run.Status == domain.ReviewRunRunning {
			return "review_running"
		}
		if run.Verdict == domain.VerdictApproved {
			return "already_approved"
		}
		if run.Verdict == domain.VerdictChangesRequested {
			return "changes_requested_same_sha"
		}
	}
	return ""
}

func triggerResultReason(prs []domain.PullRequest, result reviewcore.TriggerResult, harness domain.ReviewerHarness) string {
	for _, state := range result.Reviews {
		if state.Status == reviewcore.ReviewStateRunning {
			return "review_running"
		}
	}
	runs := make([]domain.ReviewRun, 0, len(result.Runs))
	for _, run := range result.Runs {
		if run.Harness == harness || run.Harness == "" {
			runs = append(runs, run)
		}
	}
	for _, pr := range prs {
		if blocked := existingHeadReason(runs, pr.URL, pr.HeadSHA); blocked != "" {
			return blocked
		}
	}
	return "planner_up_to_date"
}

func existingHeadReason(runs []domain.ReviewRun, prURL, targetSHA string) string {
	failedAutoRuns := 0
	for _, run := range runs {
		if run.PRURL != prURL || run.TargetSHA != targetSHA {
			continue
		}
		if run.Status == domain.ReviewRunRunning {
			return "review_running"
		}
		if run.Status == domain.ReviewRunCancelled {
			return "cancelled_same_sha"
		}
		if run.Verdict == domain.VerdictApproved {
			return "already_approved"
		}
		if run.Verdict == domain.VerdictChangesRequested {
			return "changes_requested_same_sha"
		}
		if run.Status == domain.ReviewRunFailed && run.TriggerSource == domain.ReviewTriggerAuto {
			failedAutoRuns++
		}
	}
	if failedAutoRuns >= autoReviewFailedRetryLimit {
		return "failed_same_sha_retry_limit"
	}
	return ""
}

func effectiveReviewerHarness(session domain.SessionRecord, config domain.ProjectConfig) domain.ReviewerHarness {
	if session.ReviewerHarness != "" {
		return session.ReviewerHarness
	}
	return config.ResolveReviewerHarness(session.Harness)
}

func ineligibleReason(prs []domain.PullRequest, url string) string {
	for _, pr := range prs {
		if pr.URL != url {
			continue
		}
		switch {
		case pr.Draft:
			return "draft_pr"
		case pr.Merged:
			return "merged_pr"
		case pr.Closed:
			return "closed_pr"
		case pr.HeadSHA == "":
			return "missing_head_sha"
		default:
			return "planner_ineligible"
		}
	}
	return "planner_ineligible"
}

func sessionGate(session domain.SessionRecord, now time.Time, threshold time.Duration) string {
	if !session.AutoReviewEnabled {
		return "disabled"
	}
	if session.Kind != domain.KindWorker {
		return "not_worker"
	}
	if session.IsTerminated {
		return "terminated"
	}
	if session.Activity.State != domain.ActivityIdle {
		return "not_idle"
	}
	if session.Activity.LastActivityAt.IsZero() || now.Sub(session.Activity.LastActivityAt) < threshold {
		return "idle_threshold_not_met"
	}
	return ""
}

// Sweep evaluates every known session, isolating per-session failures.
func (c *Coordinator) Sweep(ctx context.Context) error {
	sessions, err := c.store.ListAllSessions(ctx)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		if reason := sessionGate(session, c.clock(), c.idleThreshold); reason != "" {
			continue
		}
		if _, err := c.EvaluateSession(ctx, session.ID); err != nil {
			c.logger.Error("auto-review: evaluate session failed", "session_id", session.ID, "err", err)
		}
	}
	return nil
}

// Start evaluates persisted session and PR facts on the configured cadence
// until ctx is cancelled.
func (c *Coordinator) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(c.sweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.Sweep(ctx); err != nil {
					c.logger.Error("auto-review: sweep failed", "err", err)
				}
			}
		}
	}()
	return done
}
