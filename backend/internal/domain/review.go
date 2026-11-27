package domain

import (
	"errors"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/pkg/contract"
)

// ErrDuplicateReviewRun is returned by InsertReviewRun when a run already exists
// for the same worker session and target commit (the partial unique index from
// migration 0013). It lets the review engine fall back to the recorded run
// instead of surfacing a raw storage error after a reviewer may have launched.
var ErrDuplicateReviewRun = errors.New("domain: review run already exists for session and target sha")

// Review is the per-worker, per-reviewer-harness code-review record. A repeat
// trigger for the same harness reuses this row; the per-pass facts live on
// ReviewRun.
type Review struct {
	ID        string          `json:"id"`
	SessionID SessionID       `json:"sessionId"`
	ProjectID ProjectID       `json:"projectId"`
	Harness   ReviewerHarness `json:"harness"`
	PRURL     string          `json:"prUrl"`
	// ReviewerHandleID is the runtime handle of the live reviewer pane, reused
	// across passes and exposed so the UI can attach its terminal.
	ReviewerHandleID string `json:"reviewerHandleId"`
	AgentSessionID   string `json:"agentSessionId"`
	// ReviewerLaunchID is the AO runtime generation that owns the live reviewer
	// pane on this row. It fences delayed hooks from an older replaced reviewer.
	ReviewerLaunchID string `json:"-"`
	// ReviewerActivityState is the latest activity hook reported by the review
	// pane itself. It is separate from ReviewRun.Status so the UI can distinguish
	// "review pass exists" from "reviewer is actively working right now".
	ReviewerActivityState ActivityState `json:"reviewerActivityState,omitempty"`
	// InterfaceMode selects the durable reviewer surface. Chat reviewers own a
	// native conversation; TUI reviewers continue to use their terminal handle.
	InterfaceMode          ReviewerInterfaceMode `json:"interfaceMode"`
	ProviderConversationID string                `json:"providerConversationId"`
	ControllerGeneration   string                `json:"controllerGeneration"`
	ControllerError        string                `json:"controllerError"`
	CreatedAt              time.Time             `json:"createdAt"`
	UpdatedAt              time.Time             `json:"updatedAt"`

	// IsArchived retires the reviewer surface without deleting its history.
	IsArchived bool `json:"-"`
}

// ReviewerInterfaceMode selects the durable UI surface for a reviewer.
type ReviewerInterfaceMode string

const (
	// ReviewerInterfaceTUI uses the reviewer's terminal handle.
	ReviewerInterfaceTUI ReviewerInterfaceMode = "tui"
	// ReviewerInterfaceChat uses a reviewer-owned native chat.
	ReviewerInterfaceChat ReviewerInterfaceMode = "chat"
)

// ReviewerSurface gives clients one stable identifier for either reviewer UI.
type ReviewerSurface struct {
	Mode            ReviewerInterfaceMode `json:"mode"`
	ReviewID        string                `json:"reviewId"`
	Harness         ReviewerHarness       `json:"harness"`
	HandleID        string                `json:"handleId,omitempty"`
	ControllerError string                `json:"controllerError,omitempty"`
}

// ReviewRun is one review pass against a worker's PR.
type ReviewRun struct {
	ID        string    `json:"id"`
	ReviewID  string    `json:"reviewId"`
	SessionID SessionID `json:"sessionId"`
	// BatchID groups review runs created by one trigger so worker feedback can
	// be delivered once after the whole trigger batch is terminal. Empty marks
	// legacy/single-run delivery.
	BatchID string          `json:"batchId"`
	Harness ReviewerHarness `json:"harness"`
	// TriggerSource records whether this pass was requested by a user, by an
	// AO agent session through the CLI, or by the daemon auto-review coordinator.
	TriggerSource ReviewTriggerSource `json:"triggerSource" enum:"manual,agent,auto"`
	PRURL         string              `json:"prUrl"`
	// TargetSHA is the PR head commit this pass reviewed.
	TargetSHA string          `json:"targetSha"`
	Status    ReviewRunStatus `json:"status"`
	Verdict   ReviewVerdict   `json:"verdict"`
	// Body is the review text the reviewer submitted. It is recorded for AO's
	// own tracking; the reviewer also posts the review to the PR itself.
	Body string `json:"body"`
	// GithubReviewID is the id of the GitHub PR review the reviewer posted for
	// this pass (the `gh api .../pulls/{n}/reviews` object id), recorded at
	// submit time. It is empty when the reviewer could not post to the provider.
	// When the pass requests changes, AO includes it in the message to the
	// worker so the worker knows exactly which review to address and reply to.
	GithubReviewID string     `json:"githubReviewId"`
	CreatedAt      time.Time  `json:"createdAt"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
	DeliveredAt    *time.Time `json:"deliveredAt,omitempty"`
	// AutoInjectReview snapshots the session policy when this result is first
	// recorded. Later toggle changes must not rewrite or deliver this run.
	AutoInjectReview bool `json:"autoInjectReview"`
}

// ReviewTriggerSource identifies who initiated a review pass.
type ReviewTriggerSource string

const (
	// ReviewTriggerManual marks a user-initiated review pass.
	ReviewTriggerManual ReviewTriggerSource = "manual"
	// ReviewTriggerAgent marks a pass an AO agent session (a worker reviewing
	// its own PR, or an orchestrator) requested through `ao review trigger`.
	ReviewTriggerAgent ReviewTriggerSource = "agent"
	// ReviewTriggerAuto marks a daemon-initiated review pass.
	ReviewTriggerAuto ReviewTriggerSource = "auto"
)

// ReviewRunStatus is the lifecycle state of a single review pass.
type ReviewRunStatus = contract.AOReviewRunStatus

// Review run statuses.
const (
	ReviewRunRunning   = contract.AOReviewRunRunning
	ReviewRunComplete  = contract.AOReviewRunComplete
	ReviewRunDelivered = contract.AOReviewRunDelivered
	ReviewRunFailed    = contract.AOReviewRunFailed
	ReviewRunCancelled = contract.AOReviewRunCancelled
)

// ReviewVerdict is the outcome a reviewer reports. The empty verdict marks a
// run that has not produced an outcome yet.
type ReviewVerdict = contract.AOReviewVerdict

// Review verdicts.
const (
	VerdictNone             = contract.AOReviewVerdictNone
	VerdictApproved         = contract.AOReviewVerdictApproved
	VerdictChangesRequested = contract.AOReviewVerdictChangesRequested
)

// CurrentHeadReviewRun is one AO review pass recorded against a PR's current
// head commit, reduced to the fields a derived read model needs.
type CurrentHeadReviewRun struct {
	SessionID SessionID
	Harness   ReviewerHarness
	PRURL     string
	Status    ReviewRunStatus
	Verdict   ReviewVerdict
	Body      string
	ID        string
	CreatedAt time.Time
	// FailedAutoRuns counts every failed auto review pass on this PR head.
	FailedAutoRuns int
	// AutoRetryBlocked reports a cancelled pass or a verdict on this PR head for
	// this harness, either of which stops auto review from retrying the head.
	AutoRetryBlocked bool
}

// Auto-review bounded exponential backoff defaults for recoverable failures.
const (
	AutoReviewRetryBaseDelay = time.Minute
	AutoReviewRetryMaxDelay  = 30 * time.Minute
)

// AutoReviewFailureClass categorizes a failed auto-review run so recovery
// policy can distinguish transient errors, quota exhaustion, and permanent
// configuration/authentication failures.
type AutoReviewFailureClass string

// Auto-review failure classifications.
const (
	AutoReviewFailureTransient AutoReviewFailureClass = "transient"
	AutoReviewFailureQuota     AutoReviewFailureClass = "quota"
	AutoReviewFailurePermanent AutoReviewFailureClass = "permanent"
)

// ReviewRunQuotaExhaustedBody is the durable review_run body recorded when a
// reviewer Chat turn fails with typed failure_code = 'quota_exhausted'.
const ReviewRunQuotaExhaustedBody = "reviewer Chat turn failed: quota_exhausted"

// Durable review_run bodies recorded when a reviewer Chat turn fails with a
// typed permanent authentication/configuration failure_code.
const (
	ReviewRunAuthRequiredBody       = "reviewer Chat turn failed: auth_required"
	ReviewRunProviderKeyMissingBody = "reviewer Chat turn failed: provider_key_missing"
)

// IsQuotaFailedReviewRun reports whether a failed review run was caused by
// provider quota exhaustion rather than an ordinary review failure.
func IsQuotaFailedReviewRun(run ReviewRun) bool {
	return run.Status == ReviewRunFailed && run.Body == ReviewRunQuotaExhaustedBody
}

// ClassifyAutoReviewFailure classifies a failed review run into quota,
// permanent configuration/authentication, or transient failure.
func ClassifyAutoReviewFailure(run ReviewRun) AutoReviewFailureClass {
	if IsQuotaFailedReviewRun(run) {
		return AutoReviewFailureQuota
	}
	if run.Body == ReviewRunAuthRequiredBody || run.Body == ReviewRunProviderKeyMissingBody {
		return AutoReviewFailurePermanent
	}
	body := strings.ToLower(strings.TrimSpace(run.Body))
	if body == "" {
		return AutoReviewFailureTransient
	}
	permanentMarkers := []string{
		"reviewer preflight:",
		"unknown reviewer harness",
		"does not support chat",
		"invalid api key",
		"invalid_api_key",
		"authentication failed",
		"unauthorized",
		"forbidden",
		"permission denied",
		"not logged in",
		"login required",
		"missing credentials",
		"invalid configuration",
		"status 401",
		"status 403",
		"http 401",
		"http 403",
	}
	for _, marker := range permanentMarkers {
		if strings.Contains(body, marker) {
			return AutoReviewFailurePermanent
		}
	}
	return AutoReviewFailureTransient
}

// AutoReviewRetryBackoff returns the bounded exponential backoff delay for the
// given 1-based failed auto-review attempt count.
func AutoReviewRetryBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return AutoReviewRetryBaseDelay
	}
	delay := AutoReviewRetryBaseDelay
	for i := 1; i < attempt; i++ {
		if delay >= AutoReviewRetryMaxDelay/2 {
			return AutoReviewRetryMaxDelay
		}
		delay *= 2
	}
	if delay > AutoReviewRetryMaxDelay {
		return AutoReviewRetryMaxDelay
	}
	return delay
}

// AutoReviewHeadDecision is the shared eligibility and retry/deferral decision
// for automatic review of a single (prURL, targetSHA, harness) tuple.
type AutoReviewHeadDecision struct {
	Blocked      bool
	Permanent    bool
	Reason       string
	RetryAt      time.Time
	AttemptCount int
	Detail       string
}

// EvaluateAutoReviewHead evaluates whether automatic review for (prURL,
// targetSHA, harness) is currently blocked, deferred in backoff, permanently
// blocked by a config/auth error, or eligible to run at now.
//
// Both the auto-review coordinator and the review engine call this function so
// eligibility, backoff, and permanent-failure classification never drift.
func EvaluateAutoReviewHead(
	runs []ReviewRun,
	prURL, targetSHA string,
	harness ReviewerHarness,
	now time.Time,
	ignoreQuotaFailures bool,
) AutoReviewHeadDecision {
	failedAutoRuns := 0
	var latestFailed ReviewRun
	var latestFailedAt time.Time
	hasLatestFailed := false

	for _, run := range runs {
		if run.PRURL != prURL || run.TargetSHA != targetSHA {
			continue
		}
		if harness != "" && run.Harness != harness && run.Harness != "" {
			continue
		}
		if run.Status == ReviewRunRunning {
			return AutoReviewHeadDecision{Blocked: true, Reason: "review_running"}
		}
		if run.Status == ReviewRunCancelled {
			return AutoReviewHeadDecision{Blocked: true, Reason: "cancelled_same_sha"}
		}
		if run.Verdict == VerdictApproved {
			return AutoReviewHeadDecision{Blocked: true, Reason: "already_approved"}
		}
		if run.Verdict == VerdictChangesRequested {
			return AutoReviewHeadDecision{Blocked: true, Reason: "changes_requested_same_sha"}
		}
		if run.Status == ReviewRunFailed && run.TriggerSource == ReviewTriggerAuto {
			if ignoreQuotaFailures && IsQuotaFailedReviewRun(run) {
				continue
			}
			failedAutoRuns++
			failedAt := run.CreatedAt
			if run.CompletedAt != nil && !run.CompletedAt.IsZero() {
				failedAt = *run.CompletedAt
			}
			if !hasLatestFailed || failedAt.After(latestFailedAt) ||
				(failedAt.Equal(latestFailedAt) && failedAt.IsZero()) {
				latestFailed = run
				latestFailedAt = failedAt
				hasLatestFailed = true
			}
		}
	}

	if failedAutoRuns == 0 {
		return AutoReviewHeadDecision{}
	}

	detail := strings.TrimSpace(latestFailed.Body)
	if ClassifyAutoReviewFailure(latestFailed) == AutoReviewFailurePermanent {
		return AutoReviewHeadDecision{
			Blocked:      true,
			Permanent:    true,
			Reason:       "permanent_review_failure",
			AttemptCount: failedAutoRuns,
			Detail:       detail,
		}
	}

	if !latestFailedAt.IsZero() {
		retryAt := latestFailedAt.Add(AutoReviewRetryBackoff(failedAutoRuns))
		if now.Before(retryAt) {
			return AutoReviewHeadDecision{
				Blocked:      true,
				Reason:       "auto_review_retry_backoff",
				RetryAt:      retryAt,
				AttemptCount: failedAutoRuns,
				Detail:       detail,
			}
		}
	}

	return AutoReviewHeadDecision{
		Blocked:      false,
		AttemptCount: failedAutoRuns,
		Detail:       detail,
	}
}

// ReviewerHandle names one live reviewer pane's runtime handle, independent
// of whether the worker session that spawned it still exists. A reviewer has
// no session row of its own — its identity is this review's id and
// SessionID (the worker it reviews) plus Harness — so the memory diagnostic
// needs this to find it by anything other than walking session roots.
type ReviewerHandle struct {
	ReviewID  string
	SessionID SessionID
	Harness   ReviewerHarness
	HandleID  string
}
