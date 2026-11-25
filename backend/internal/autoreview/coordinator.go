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

	deferMu         sync.Mutex
	deferredUntil   map[domain.SessionID]time.Time
	lastDeferralSig map[domain.SessionID]string
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
		routeReady:      cfg.RouteReady,
		deferredUntil:   make(map[domain.SessionID]time.Time),
		lastDeferralSig: make(map[domain.SessionID]string),
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
		// Always evaluate current routes and fresh quota evidence so positive
		// quota recovery or newly configured fallbacks can wake a deferred HEAD.
		deferredRetryAt, hasActiveDeferral := c.activeDeferral(ctx, id, prs, now)
		decision := c.selectReviewerRoute(ctx, id, project, harness, prs, runs, now, hasActiveDeferral)
		if decision.reason != "" {
			if decision.reason == "reviewer_quota_deferred" {
				c.persistDeferrals(ctx, id, prs, decision.retryAt, decision.reason, now)
				c.logDeferralDecision(id, decision, hasActiveDeferral)
				return Result{Reason: decision.reason, RetryAt: decision.retryAt}, nil
			}
			if hasActiveDeferral {
				c.logger.Debug("auto-review: retaining active reviewer quota deferral",
					"role", workerquota.RoleReviewer, "session_id", id,
					"retry_at", deferredRetryAt, "route_reason", decision.reason,
					"total_routes", decision.totalRoutes,
					"exhausted_routes", decision.exhaustedRoutes,
					"unready_routes", decision.unreadyRoutes,
					"retry_capped_routes", decision.retryCappedRoutes,
					"unknown_during_deferral_routes", decision.unknownDuringDeferralRoutes)
				return Result{Reason: "reviewer_quota_deferred", RetryAt: deferredRetryAt}, nil
			}
			return Result{Reason: decision.reason}, nil
		}
		if hasActiveDeferral && !decision.freshPositive {
			// Retain persisted deferral when no fresh positive quota evidence
			// is available (e.g. Quotum temporarily unavailable after restart).
			c.logger.Debug("auto-review: retaining active reviewer quota deferral; no fresh positive evidence",
				"role", workerquota.RoleReviewer, "session_id", id, "retry_at", deferredRetryAt)
			return Result{Reason: "reviewer_quota_deferred", RetryAt: deferredRetryAt}, nil
		}
		if hasActiveDeferral && decision.freshPositive {
			c.clearDeferrals(ctx, id, prs)
		}
		if decision.route.Harness != "" {
			selectedRoute = &decision.route
			harness = domain.ReviewerHarness(decision.route.Harness)
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
	quotaManaged := selectedRoute != nil
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
			if blocked := existingHeadReason(harnessRuns, state.PRURL, state.TargetSHA, quotaManaged); blocked != "" {
				reason = blocked
				continue
			}
			// When quota failover selects a different reviewer harness, never
			// duplicate a review that is already running or decided on this HEAD.
			if quotaManaged {
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
		return Result{Reason: triggerResultReason(prs, result, harness, quotaManaged)}, nil
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
	c.deferMu.Lock()
	delete(c.lastDeferralSig, id)
	c.deferMu.Unlock()
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

type routeDecision struct {
	route                       workerquota.Target
	retryAt                     time.Time
	retryAtSource               string
	reason                      string
	freshPositive               bool
	totalRoutes                 int
	exhaustedRoutes             int
	unreadyRoutes               int
	retryCappedRoutes           int
	unknownDuringDeferralRoutes int
}

type routeWindowSummary struct {
	matchedWindows int
	evidence       string
	observedAt     time.Time
	validUntil     time.Time
	resetAt        time.Time
}

func summarizeRouteWindows(windows []workerquota.Window, now time.Time) routeWindowSummary {
	summary := routeWindowSummary{
		matchedWindows: len(windows),
		evidence:       "unknown",
	}
	exhausted := false
	freshPositive := false
	for _, w := range windows {
		if !w.ObservedAt.IsZero() && (summary.observedAt.IsZero() || w.ObservedAt.After(summary.observedAt)) {
			summary.observedAt = w.ObservedAt
		}
		active := !w.ObservedAt.IsZero() && !w.ObservedAt.After(now) && now.Before(w.ValidUntil)
		if active {
			if summary.validUntil.IsZero() || w.ValidUntil.Before(summary.validUntil) {
				summary.validUntil = w.ValidUntil
			}
			if !w.ResetAt.IsZero() && now.Before(w.ResetAt) {
				if summary.resetAt.IsZero() || w.ResetAt.Before(summary.resetAt) {
					summary.resetAt = w.ResetAt
				}
			}
			if w.Exhausted && (w.ResetAt.IsZero() || now.Before(w.ResetAt)) {
				exhausted = true
			} else if !w.Exhausted {
				freshPositive = true
			}
		} else if !w.ValidUntil.IsZero() && summary.validUntil.IsZero() {
			summary.validUntil = w.ValidUntil
		}
	}
	switch {
	case exhausted:
		summary.evidence = "exhausted"
	case freshPositive:
		summary.evidence = "available"
	}
	return summary
}

func computeRetryDeadline(targets []workerquota.Target, windows map[workerquota.Target][]workerquota.Window, now time.Time, sweepInterval time.Duration) (time.Time, string) {
	var earliest time.Time
	source := "sweep_interval"
	for _, target := range targets {
		for _, w := range windows[target] {
			if !w.Exhausted || w.ObservedAt.IsZero() || w.ObservedAt.After(now) ||
				!now.Before(w.ValidUntil) || (!w.ResetAt.IsZero() && !now.Before(w.ResetAt)) {
				continue
			}
			candidate := w.ResetAt
			candidateSource := "quota_reset"
			if candidate.IsZero() || w.ValidUntil.Before(candidate) {
				candidate = w.ValidUntil
				candidateSource = "snapshot_expiry"
			}
			if candidate.After(now) && (earliest.IsZero() || candidate.Before(earliest)) {
				earliest = candidate
				source = candidateSource
			}
		}
	}
	if earliest.IsZero() {
		return now.Add(sweepInterval), "sweep_interval"
	}
	return earliest, source
}

func (c *Coordinator) logDeferralDecision(id domain.SessionID, d routeDecision, hasActiveDeferral bool) {
	allExhausted := d.totalRoutes > 0 && d.exhaustedRoutes == d.totalRoutes
	msg := "auto-review: no eligible reviewer quota route available; deferring"
	if allExhausted {
		msg = "auto-review: all reviewer quota routes exhausted; deferring"
	}
	sig := fmt.Sprintf("%s|%s|%t|%d|%d|%d|%d|%d",
		d.retryAt.UTC().Format(time.RFC3339), d.retryAtSource, allExhausted,
		d.totalRoutes, d.exhaustedRoutes, d.unreadyRoutes, d.retryCappedRoutes, d.unknownDuringDeferralRoutes)
	c.deferMu.Lock()
	changed := c.lastDeferralSig[id] != sig
	c.lastDeferralSig[id] = sig
	c.deferMu.Unlock()

	args := []any{
		"role", workerquota.RoleReviewer,
		"session_id", id,
		"retry_at", d.retryAt,
		"retry_at_source", d.retryAtSource,
		"active_deferral", hasActiveDeferral,
		"all_routes_exhausted", allExhausted,
		"total_routes", d.totalRoutes,
		"exhausted_routes", d.exhaustedRoutes,
		"unready_routes", d.unreadyRoutes,
		"retry_capped_routes", d.retryCappedRoutes,
		"unknown_during_deferral_routes", d.unknownDuringDeferralRoutes,
	}
	if changed {
		c.logger.Info(msg, args...)
	} else {
		c.logger.Debug(msg, args...)
	}
}

func (c *Coordinator) selectReviewerRoute(ctx context.Context, sessionID domain.SessionID, project domain.ProjectRecord, projectHarness domain.ReviewerHarness, prs []domain.PullRequest, runs []domain.ReviewRun, now time.Time, requireFreshPositive bool) routeDecision {
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
		return routeDecision{}
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
	decision := routeDecision{totalRoutes: len(targets)}
	logRoute := func(route workerquota.Target, summary routeWindowSummary, freshPositive bool, outcome string) {
		c.logger.Debug("auto-review: reviewer quota route evaluated",
			"role", workerquota.RoleReviewer,
			"session_id", sessionID,
			"harness", route.Harness,
			"model", route.Model,
			"effort", route.Effort,
			"mode", route.Mode,
			"account", route.Account,
			"matched_windows", summary.matchedWindows,
			"evidence", summary.evidence,
			"fresh_positive", freshPositive,
			"observed_at", summary.observedAt,
			"valid_until", summary.validUntil,
			"reset_at", summary.resetAt,
			"active_deferral", requireFreshPositive,
			"decision", outcome)
	}
	blocked := false
	for _, route := range targets {
		routeWindows := windows[route]
		summary := summarizeRouteWindows(routeWindows, now)
		freshPositive := hasFreshPositiveWindow(routeWindows, now)
		if _, ok := workerquota.Select([]workerquota.Target{route}, windows, now); !ok {
			blocked = true
			decision.exhaustedRoutes++
			logRoute(route, summary, freshPositive, "quota_exhausted")
			continue
		}
		if c.routeReady != nil && !c.routeReady(ctx, route) {
			decision.unreadyRoutes++
			logRoute(route, summary, freshPositive, "route_not_ready")
			continue
		}
		// If this route's harness has already hit the per-SHA auto-retry cap on
		// every pending PR, yield to the next eligible fallback route instead of
		// stranding the pending HEAD.
		if allPendingPRsRetryCapped(prs, runs, domain.ReviewerHarness(route.Harness)) {
			decision.retryCappedRoutes++
			logRoute(route, summary, freshPositive, "retry_capped")
			continue
		}
		// During an active durable deferral, do not stop on an earlier route
		// whose quota is merely unknown; keep scanning for a later route with
		// fresh positive capacity.
		if requireFreshPositive && !freshPositive {
			decision.unknownDuringDeferralRoutes++
			logRoute(route, summary, freshPositive, "deferral_requires_fresh_positive")
			continue
		}
		logRoute(route, summary, freshPositive, "selected")
		if route != primary || route.Mode != "" || route.Account != "" {
			c.logger.Info("auto-review: reviewer quota selected configured route",
				"role", workerquota.RoleReviewer, "session_id", sessionID, "project", project.ID,
				"harness", route.Harness, "model", route.Model, "effort", route.Effort,
				"mode", route.Mode, "account", route.Account,
				"evidence", summary.evidence, "fresh_positive", freshPositive)
		}
		decision.route = route
		decision.freshPositive = freshPositive
		return decision
	}
	if blocked {
		decision.retryAt, decision.retryAtSource = computeRetryDeadline(targets, windows, now, c.sweepInterval)
		decision.reason = "reviewer_quota_deferred"
		return decision
	}
	decision.reason = "reviewer_quota_no_ready_route"
	return decision
}

func allPendingPRsRetryCapped(prs []domain.PullRequest, runs []domain.ReviewRun, harness domain.ReviewerHarness) bool {
	pending := 0
	capped := 0
	for _, pr := range prs {
		if pr.URL == "" || pr.HeadSHA == "" || pr.Draft || pr.Merged || pr.Closed {
			continue
		}
		if crossHarnessHeadReason(runs, pr.URL, pr.HeadSHA) != "" {
			continue
		}
		pending++
		harnessRuns := make([]domain.ReviewRun, 0, len(runs))
		for _, r := range runs {
			if r.Harness == harness || r.Harness == "" {
				harnessRuns = append(harnessRuns, r)
			}
		}
		if existingHeadReason(harnessRuns, pr.URL, pr.HeadSHA, true) == "failed_same_sha_retry_limit" {
			capped++
		}
	}
	return pending > 0 && capped == pending
}

func hasFreshPositiveWindow(windows []workerquota.Window, now time.Time) bool {
	for _, w := range windows {
		if !w.Exhausted && !w.ObservedAt.IsZero() && !w.ObservedAt.After(now) && now.Before(w.ValidUntil) {
			return true
		}
	}
	return false
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

func triggerResultReason(prs []domain.PullRequest, result reviewcore.TriggerResult, harness domain.ReviewerHarness, quotaManaged bool) string {
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
		if blocked := existingHeadReason(runs, pr.URL, pr.HeadSHA, quotaManaged); blocked != "" {
			return blocked
		}
	}
	return "planner_up_to_date"
}

func existingHeadReason(runs []domain.ReviewRun, prURL, targetSHA string, quotaManaged ...bool) string {
	ignoreQuotaFailures := len(quotaManaged) > 0 && quotaManaged[0]
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
		if run.Status == domain.ReviewRunFailed && run.TriggerSource == domain.ReviewTriggerAuto && (!ignoreQuotaFailures || !domain.IsQuotaFailedReviewRun(run)) {
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
