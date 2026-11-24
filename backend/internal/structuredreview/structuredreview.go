// Package structuredreview parses and evaluates structured AO review-body
// decisions on submitted pull request reviews alongside native SCM reviews.
package structuredreview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Source identifies whether an effective review decision came from native SCM
// review state or from a structured AO-REVIEW review body.
type Source string

const (
	SourceNative     Source = "native"
	SourceStructured Source = "structured"
)

// DeliveryStatus distinguishes whether an active structured feedback item has
// been durably handed off to a worker versus still pending delivery.
type DeliveryStatus string

const (
	DeliveryPending   DeliveryStatus = "pending"
	DeliveryDelivered DeliveryStatus = "delivered"
)

const (
	headerDecisionPrefix = "AO-REVIEW:"
	headerHeadPrefix     = "HEAD:"
	nudgeKeyPrefix       = "structured-review:"
)

// Policy controls whether structured review-body decisions are enabled and
// which reviewer logins are authorized to emit them.
type Policy struct {
	Enabled          bool
	TrustedReviewers []string
	AllowPRAuthor    bool
}

// IsAuthorized reports whether reviewer is authorized under p to issue a
// structured review-body decision on a PR authored by prAuthor.
func (p Policy) IsAuthorized(reviewer, prAuthor string) bool {
	if !p.Enabled {
		return false
	}
	normReviewer := normalizeLogin(reviewer)
	if normReviewer == "" {
		return false
	}
	if p.AllowPRAuthor {
		if normAuthor := normalizeLogin(prAuthor); normAuthor != "" && strings.EqualFold(normReviewer, normAuthor) {
			return true
		}
	}
	for _, trusted := range p.TrustedReviewers {
		if normTrusted := normalizeLogin(trusted); normTrusted != "" && strings.EqualFold(normReviewer, normTrusted) {
			return true
		}
	}
	return false
}

// ParsedReview is the validated structured header and feedback extracted from a
// submitted review body.
type ParsedReview struct {
	Decision domain.ReviewDecision
	HeadSHA  string
	Feedback string
}

// ParseBody parses a strict structured review-body header at the very beginning
// of body. Ordinary prose, quoted markers, code blocks, malformed headers,
// short/non-hex SHAs, and empty REQUEST_CHANGES bodies are rejected.
func ParseBody(body string) (ParsedReview, bool) {
	if body == "" {
		return ParsedReview{}, false
	}
	normalized := strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) < 2 {
		return ParsedReview{}, false
	}

	line1 := strings.TrimRight(lines[0], " \t")
	if !strings.HasPrefix(line1, headerDecisionPrefix) {
		return ParsedReview{}, false
	}
	rawDecision := strings.TrimSpace(strings.TrimPrefix(line1, headerDecisionPrefix))
	var decision domain.ReviewDecision
	switch rawDecision {
	case "REQUEST_CHANGES":
		decision = domain.ReviewChangesRequest
	case "APPROVE":
		decision = domain.ReviewApproved
	default:
		return ParsedReview{}, false
	}

	line2 := strings.TrimRight(lines[1], " \t")
	if !strings.HasPrefix(line2, headerHeadPrefix) {
		return ParsedReview{}, false
	}
	rawSHA := strings.TrimSpace(strings.TrimPrefix(line2, headerHeadPrefix))
	if !isFullCommitSHA(rawSHA) {
		return ParsedReview{}, false
	}
	headSHA := strings.ToLower(rawSHA)

	var feedback string
	if len(lines) > 2 {
		feedback = strings.TrimSpace(strings.Join(lines[2:], "\n"))
	}
	if decision == domain.ReviewChangesRequest && feedback == "" {
		return ParsedReview{}, false
	}

	return ParsedReview{
		Decision: decision,
		HeadSHA:  headSHA,
		Feedback: feedback,
	}, true
}

// NudgeKey returns the stable durable reaction key for a structured review.
func NudgeKey(prURL, reviewID string) string {
	return nudgeKeyPrefix + prURL + ":" + reviewID
}

// Signature returns a deterministic idempotency signature for a structured
// review decision so repeated polls, overlapping observers, and restarts do not
// re-deliver the same feedback.
func Signature(decision domain.ReviewDecision, reviewedSHA, feedback string) string {
	h := sha256.New()
	h.Write([]byte("v1\x00"))
	h.Write([]byte(decision))
	h.Write([]byte("\x00"))
	h.Write([]byte(strings.ToLower(strings.TrimSpace(reviewedSHA))))
	h.Write([]byte("\x00"))
	h.Write([]byte(strings.TrimSpace(feedback)))
	return hex.EncodeToString(h.Sum(nil))
}

// EvalInput holds the facts needed to evaluate native and structured review
// decisions for a pull request.
type EvalInput struct {
	Policy               Policy
	PRAuthor             string
	CurrentHeadSHA       string
	GitHubDecision       domain.ReviewDecision
	Reviews              []domain.PullRequestReview
	HasUnresolvedThreads bool
}

// EffectiveReviewerDecision is the latest effective decision for one reviewer.
type EffectiveReviewerDecision struct {
	Reviewer    string
	Source      Source
	Decision    domain.ReviewDecision
	ReviewID    string
	ReviewURL   string
	ReviewedSHA string
	Feedback    string
	SubmittedAt time.Time
	Stale       bool
}

// ActiveChangeRequest is an unsuperseded structured REQUEST_CHANGES review
// that requires worker rework.
type ActiveChangeRequest struct {
	Review      domain.PullRequestReview
	ReviewedSHA string
	Feedback    string
	Signature   string
}

// Evaluation is the deterministic result of combining native GitHub review
// state with authorized structured review-body decisions.
type Evaluation struct {
	GitHubDecision       domain.ReviewDecision
	EffectiveDecision    domain.ReviewDecision
	Source               Source
	ReviewedSHA          string
	CurrentHeadSHA       string
	ActiveChangeRequests []ActiveChangeRequest
	PerReviewer          []EffectiveReviewerDecision
	HasUnresolvedThreads bool
}

// Evaluate computes per-reviewer effective decisions and the aggregate AO
// decision while keeping GitHub's native decision separate.
func Evaluate(in EvalInput) Evaluation {
	githubDecision := in.GitHubDecision
	if githubDecision == "" {
		githubDecision = domain.ReviewNone
	}
	currentHead := strings.ToLower(strings.TrimSpace(in.CurrentHeadSHA))

	ordered := make([]domain.PullRequestReview, len(in.Reviews))
	copy(ordered, in.Reviews)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].SubmittedAt.Equal(ordered[j].SubmittedAt) {
			return ordered[i].SubmittedAt.Before(ordered[j].SubmittedAt)
		}
		return ordered[i].ID < ordered[j].ID
	})

	byReviewer := make(map[string]EffectiveReviewerDecision)
	activeReqByReviewer := make(map[string]ActiveChangeRequest)

	for _, r := range ordered {
		reviewerKey := strings.ToLower(normalizeLogin(r.Author))
		if reviewerKey == "" {
			continue
		}
		switch r.State {
		case domain.ReviewApproved, domain.ReviewChangesRequest:
			byReviewer[reviewerKey] = EffectiveReviewerDecision{
				Reviewer:    normalizeLogin(r.Author),
				Source:      SourceNative,
				Decision:    r.State,
				ReviewID:    r.ID,
				ReviewURL:   r.URL,
				ReviewedSHA: strings.ToLower(strings.TrimSpace(r.TargetSHA)),
				Feedback:    strings.TrimSpace(r.Body),
				SubmittedAt: r.SubmittedAt,
			}
			delete(activeReqByReviewer, reviewerKey)
		case domain.ReviewNone, "":
			if !in.Policy.IsAuthorized(r.Author, in.PRAuthor) {
				continue
			}
			if strings.TrimSpace(r.ID) == "" {
				continue
			}
			targetSHA := strings.ToLower(strings.TrimSpace(r.TargetSHA))
			if !isFullCommitSHA(targetSHA) {
				continue
			}
			parsed, ok := ParseBody(r.Body)
			if !ok {
				continue
			}
			if parsed.HeadSHA != targetSHA {
				continue
			}
			switch parsed.Decision {
			case domain.ReviewChangesRequest:
				byReviewer[reviewerKey] = EffectiveReviewerDecision{
					Reviewer:    normalizeLogin(r.Author),
					Source:      SourceStructured,
					Decision:    domain.ReviewChangesRequest,
					ReviewID:    r.ID,
					ReviewURL:   r.URL,
					ReviewedSHA: parsed.HeadSHA,
					Feedback:    parsed.Feedback,
					SubmittedAt: r.SubmittedAt,
				}
				activeReqByReviewer[reviewerKey] = ActiveChangeRequest{
					Review:      r,
					ReviewedSHA: parsed.HeadSHA,
					Feedback:    parsed.Feedback,
					Signature:   Signature(parsed.Decision, parsed.HeadSHA, parsed.Feedback),
				}
			case domain.ReviewApproved:
				delete(activeReqByReviewer, reviewerKey)
				stale := currentHead == "" || parsed.HeadSHA != currentHead
				effDecision := domain.ReviewApproved
				if stale {
					effDecision = domain.ReviewNone
				}
				byReviewer[reviewerKey] = EffectiveReviewerDecision{
					Reviewer:    normalizeLogin(r.Author),
					Source:      SourceStructured,
					Decision:    effDecision,
					ReviewID:    r.ID,
					ReviewURL:   r.URL,
					ReviewedSHA: parsed.HeadSHA,
					Feedback:    parsed.Feedback,
					SubmittedAt: r.SubmittedAt,
					Stale:       stale,
				}
			}
		}
	}

	perReviewer := make([]EffectiveReviewerDecision, 0, len(byReviewer))
	for _, d := range byReviewer {
		perReviewer = append(perReviewer, d)
	}
	sort.Slice(perReviewer, func(i, j int) bool {
		return strings.ToLower(perReviewer[i].Reviewer) < strings.ToLower(perReviewer[j].Reviewer)
	})

	activeReqs := make([]ActiveChangeRequest, 0, len(activeReqByReviewer))
	for _, req := range activeReqByReviewer {
		activeReqs = append(activeReqs, req)
	}
	sort.Slice(activeReqs, func(i, j int) bool {
		if !activeReqs[i].Review.SubmittedAt.Equal(activeReqs[j].Review.SubmittedAt) {
			return activeReqs[i].Review.SubmittedAt.Before(activeReqs[j].Review.SubmittedAt)
		}
		return activeReqs[i].Review.ID < activeReqs[j].Review.ID
	})

	eval := Evaluation{
		GitHubDecision:       githubDecision,
		EffectiveDecision:    domain.ReviewNone,
		CurrentHeadSHA:       currentHead,
		ActiveChangeRequests: activeReqs,
		PerReviewer:          perReviewer,
		HasUnresolvedThreads: in.HasUnresolvedThreads,
	}

	// 1. Any active change request (native or structured) or native GitHub
	// CHANGES_REQUESTED always wins.
	var latestCR *EffectiveReviewerDecision
	for i := range perReviewer {
		d := &perReviewer[i]
		if d.Decision != domain.ReviewChangesRequest {
			continue
		}
		if latestCR == nil || d.SubmittedAt.After(latestCR.SubmittedAt) || (d.SubmittedAt.Equal(latestCR.SubmittedAt) && d.ReviewID > latestCR.ReviewID) {
			latestCR = d
		}
	}
	if latestCR != nil || githubDecision == domain.ReviewChangesRequest {
		eval.EffectiveDecision = domain.ReviewChangesRequest
		if latestCR != nil {
			eval.Source = latestCR.Source
			eval.ReviewedSHA = latestCR.ReviewedSHA
		} else {
			eval.Source = SourceNative
		}
		return eval
	}

	// 2. Preserve GitHub's native REVIEW_REQUIRED so a structured approval
	// cannot silently bypass branch protection or other required reviewers.
	if githubDecision == domain.ReviewRequired {
		eval.EffectiveDecision = domain.ReviewRequired
		eval.Source = SourceNative
		return eval
	}

	// 3. Preserve native GitHub APPROVED.
	if githubDecision == domain.ReviewApproved {
		eval.EffectiveDecision = domain.ReviewApproved
		eval.Source = SourceNative
		for i := range perReviewer {
			d := perReviewer[i]
			if d.Source == SourceNative && d.Decision == domain.ReviewApproved {
				eval.ReviewedSHA = d.ReviewedSHA
			}
		}
		return eval
	}

	// 4. Current-HEAD structured approval applies only when there are no
	// unresolved actionable review threads.
	if !in.HasUnresolvedThreads {
		var latestApprove *EffectiveReviewerDecision
		for i := range perReviewer {
			d := &perReviewer[i]
			if d.Source != SourceStructured || d.Decision != domain.ReviewApproved || d.Stale {
				continue
			}
			if latestApprove == nil || d.SubmittedAt.After(latestApprove.SubmittedAt) || (d.SubmittedAt.Equal(latestApprove.SubmittedAt) && d.ReviewID > latestApprove.ReviewID) {
				latestApprove = d
			}
		}
		if latestApprove != nil {
			eval.EffectiveDecision = domain.ReviewApproved
			eval.Source = SourceStructured
			eval.ReviewedSHA = latestApprove.ReviewedSHA
		}
	}

	return eval
}

// PendingFeedbackDiagnostic describes one active structured change request and
// its delivery state (delivered vs still pending handoff).
type PendingFeedbackDiagnostic struct {
	ReviewID       string         `json:"reviewId"`
	Reviewer       string         `json:"reviewer"`
	ReviewURL      string         `json:"reviewUrl,omitempty"`
	ReviewedSHA    string         `json:"reviewedSha"`
	Feedback       string         `json:"feedback"`
	DeliveryStatus DeliveryStatus `json:"deliveryStatus"`
}

// Diagnostics exposes AO's local effective decision separately from GitHub's
// native review decision, along with source, reviewed SHA, and pending feedback.
type Diagnostics struct {
	GitHubDecision    domain.ReviewDecision       `json:"githubDecision"`
	EffectiveDecision domain.ReviewDecision       `json:"effectiveDecision"`
	Source            Source                      `json:"source,omitempty"`
	ReviewedSHA       string                      `json:"reviewedSha,omitempty"`
	PendingFeedback   []PendingFeedbackDiagnostic `json:"pendingFeedback,omitempty"`
}

type nudgePayload struct {
	Seen map[string]string `json:"seen,omitempty"`
}

// BuildDiagnostics projects an Evaluation and the PR's persisted
// last_nudge_signature JSON into Diagnostics.
func BuildDiagnostics(eval Evaluation, prURL, lastNudgeSignatureRaw string) Diagnostics {
	var seen map[string]string
	if strings.TrimSpace(lastNudgeSignatureRaw) != "" {
		var p nudgePayload
		if err := json.Unmarshal([]byte(lastNudgeSignatureRaw), &p); err == nil {
			seen = p.Seen
		}
	}
	diag := Diagnostics{
		GitHubDecision:    eval.GitHubDecision,
		EffectiveDecision: eval.EffectiveDecision,
		Source:            eval.Source,
		ReviewedSHA:       eval.ReviewedSHA,
	}
	if len(eval.ActiveChangeRequests) > 0 {
		diag.PendingFeedback = make([]PendingFeedbackDiagnostic, 0, len(eval.ActiveChangeRequests))
		for _, req := range eval.ActiveChangeRequests {
			status := DeliveryPending
			if seen != nil && seen[NudgeKey(prURL, req.Review.ID)] == req.Signature {
				status = DeliveryDelivered
			}
			diag.PendingFeedback = append(diag.PendingFeedback, PendingFeedbackDiagnostic{
				ReviewID:       req.Review.ID,
				Reviewer:       normalizeLogin(req.Review.Author),
				ReviewURL:      req.Review.URL,
				ReviewedSHA:    req.ReviewedSHA,
				Feedback:       req.Feedback,
				DeliveryStatus: status,
			})
		}
	}
	return diag
}

func normalizeLogin(login string) string {
	return strings.TrimPrefix(strings.TrimSpace(login), "@")
}

func isFullCommitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
