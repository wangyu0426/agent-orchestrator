package scm

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ConversationCommentProvider is an optional provider capability for paginated
// top-level PR conversation comment retrieval.
type ConversationCommentProvider interface {
	FetchConversationComments(ctx context.Context, ref ports.SCMPRRef) (ports.SCMConversationCommentsResult, error)
}

// conversationFeedbackStore is the optional persistence capability for durable
// top-level PR conversation feedback commands.
type conversationFeedbackStore interface {
	GetPRConversationFeedbackState(ctx context.Context, prURL string) (time.Time, bool, error)
	MarkPRConversationFeedbackInitialized(ctx context.Context, prURL string, at time.Time) error
	ListPRConversationFeedback(ctx context.Context, prURL string) ([]domain.PullRequestConversationFeedback, error)
	UpsertPRConversationFeedback(ctx context.Context, item domain.PullRequestConversationFeedback) error
}

// refreshConversationFeedback polls paginated top-level PR conversation
// comments for open PRs whose project explicitly opted in to
// PRCommentFeedback, persists authorized `/ao review-feedback` commands, and
// returns the set of PR keys that have active conversation feedback requiring
// lifecycle evaluation (including on unchanged HEADs).
func (o *Observer) refreshConversationFeedback(
	ctx context.Context,
	subjects map[string]*subject,
	observations map[string]ports.SCMObservation,
	subjectsByPR map[string]*subject,
	forced map[string]bool,
	localOnlyObservations map[string]bool,
	now time.Time,
) map[string]bool {
	activePRKeys := map[string]bool{}
	ccProvider, ok := o.provider.(ConversationCommentProvider)
	if !ok {
		return activePRKeys
	}
	fbStore, ok := o.store.(conversationFeedbackStore)
	if !ok {
		return activePRKeys
	}

	for _, s := range subjects {
		if !s.hasPR || s.known.Number <= 0 {
			continue
		}
		prURL := s.known.URL
		if prURL == "" {
			continue
		}
		if s.known.Merged || s.known.Closed {
			continue
		}
		if s.session.IsTerminated {
			continue
		}
		pkey := keyForTrackedPR(s.known, s.repo)
		if obs, hasObs := observations[pkey]; hasObs && (obs.PR.Merged || obs.PR.Closed) {
			continue
		}

		proj, found, err := o.store.GetProject(ctx, string(s.session.ProjectID))
		if err != nil {
			o.logger.Warn("scm observer: load project for conversation feedback failed", "project", s.session.ProjectID, "pr", prURL, "err", err)
			continue
		}
		if !found || !proj.Config.PRCommentFeedback.Enabled {
			continue
		}
		var hasActive bool
		freshOpen := false
		obs, hasObs := observations[pkey]
		if hasObs && obs.Fetched && !obs.PR.Merged && !obs.PR.Closed {
			freshOpen = true
		}
		if o.inRateLimitCooldown(now, s.repo.Provider) || !o.needsConversationFeedbackRefresh(pkey, s.known, obs, hasObs, forced, now) {
			hasActive = o.hasPersistedActiveConversationFeedback(ctx, fbStore, prURL)
		} else {
			started := time.Now()
			comments, err := ccProvider.FetchConversationComments(ctx, ports.SCMPRRef{
				Repo:   s.repo,
				Number: s.known.Number,
				URL:    prURL,
			})
			if errors.Is(err, ports.ErrSCMUnsupported) {
				continue
			}
			o.logProviderRequest("fetch_conversation_comments", repoFullName(s.repo), started, err)
			if err != nil {
				if cooldown, ok := rateLimitCooldown(now, err); ok {
					o.setRateLimitCooldown(now, s.repo.Provider, cooldown)
					o.logger.Warn("scm observer: conversation comments rate-limited; entering cooldown while preserving persisted active rows",
						"pr", prURL, "provider", s.repo.Provider, "cooldown", cooldown, "err", err)
				} else {
					o.logger.Warn("scm observer: conversation comments refresh failed; preserving persisted active rows for retry", "pr", prURL, "err", err)
				}
				hasActive = o.hasPersistedActiveConversationFeedback(ctx, fbStore, prURL)
			} else if comments.PRMerged || comments.PRClosed {
				o.cacheSetTime(o.Cache.LastConversationPollAt, &o.Cache.lastConversationPollOrder, pkey, now)
				continue
			} else {
				freshOpen = true
				o.cacheSetTime(o.Cache.LastConversationPollAt, &o.Cache.lastConversationPollOrder, pkey, now)
				hasActive, err = o.reconcileConversationFeedback(ctx, fbStore, s, proj.Config.PRCommentFeedback, comments, now)
				if err != nil {
					o.logger.Error("scm observer: reconcile conversation feedback failed", "pr", prURL, "session", s.session.ID, "err", err)
					hasActive = o.hasPersistedActiveConversationFeedback(ctx, fbStore, prURL)
				}
			}
		}
		if !hasActive || !freshOpen {
			continue
		}

		activePRKeys[pkey] = true
		if _, hasObs := observations[pkey]; !hasObs {
			checks, err := o.store.ListChecks(ctx, prURL)
			if err != nil {
				o.logger.Warn("scm observer: list local checks for conversation feedback failed", "pr", prURL, "err", err)
			}
			obs := observationFromLocal(s.repo, s.known, checks)
			obs.ObservedAt = now
			observations[pkey] = obs
			subjectsByPR[pkey] = s
			localOnlyObservations[pkey] = true
		}
	}

	return activePRKeys
}

func (o *Observer) needsConversationFeedbackRefresh(key string, local domain.PullRequest, obs ports.SCMObservation, hasObs bool, forced map[string]bool, now time.Time) bool {
	if forcedFor(forced, local) {
		return true
	}
	last := o.Cache.LastConversationPollAt[key]
	if last.IsZero() {
		return true
	}
	if hasObs && obs.Fetched && !obs.PR.UpdatedAtProvider.IsZero() && obs.PR.UpdatedAtProvider.After(last) {
		return true
	}
	return now.Sub(last) >= o.reviewInterval
}

func (o *Observer) hasPersistedActiveConversationFeedback(ctx context.Context, fbStore conversationFeedbackStore, prURL string) bool {
	rows, err := fbStore.ListPRConversationFeedback(ctx, prURL)
	if err != nil {
		o.logger.Warn("scm observer: check persisted active conversation feedback failed", "pr", prURL, "err", err)
		return false
	}
	for _, r := range rows {
		if r.State == domain.PRConversationFeedbackActive {
			return true
		}
	}
	return false
}

func (o *Observer) reconcileConversationFeedback(
	ctx context.Context,
	fbStore conversationFeedbackStore,
	s *subject,
	cfg domain.PRCommentFeedbackConfig,
	result ports.SCMConversationCommentsResult,
	now time.Time,
) (bool, error) {
	comments := result.Comments
	prURL := s.known.URL
	_, initialized, err := fbStore.GetPRConversationFeedbackState(ctx, prURL)
	if err != nil {
		return false, err
	}

	existingRows, err := fbStore.ListPRConversationFeedback(ctx, prURL)
	if err != nil {
		return false, err
	}
	existingByID := make(map[string]domain.PullRequestConversationFeedback, len(existingRows))
	for _, r := range existingRows {
		existingByID[r.CommentID] = r
	}

	// Establish baseline cutoff so historic comments are never replayed on rollout.
	// cfg.EnabledAt always applies to newly seen comments; s.known.ObservedAt
	// applies until a complete (!result.Partial) scan has initialized the PR.
	var cutoff time.Time
	if cfg.EnabledAt != nil && !cfg.EnabledAt.IsZero() {
		cutoff = cfg.EnabledAt.UTC()
	}
	if !initialized && !s.known.ObservedAt.IsZero() && s.known.ObservedAt.UTC().After(cutoff) {
		cutoff = s.known.ObservedAt.UTC()
	}

	presentIDs := make(map[string]bool, len(comments))
	var activeCandidates []domain.PullRequestConversationFeedback

	for _, c := range comments {
		if c.ID == "" {
			continue
		}
		presentIDs[c.ID] = true

		parsed, ok, parseReason := domain.ParsePRFeedbackCommand(c.Body)
		if !ok {
			if parseReason != "no_marker" {
				o.logger.Info("scm observer: conversation feedback ignored",
					"event", "ignored",
					"pr", prURL,
					"session", s.session.ID,
					"comment_id", c.ID,
					"author", c.Author,
					"reason", parseReason,
				)
			}
			// If an active command was edited to remove the marker or empty the body,
			// mark it deleted so pending retries stop.
			if prev, exists := existingByID[c.ID]; exists && prev.State == domain.PRConversationFeedbackActive {
				prev.State = domain.PRConversationFeedbackDeleted
				prev.UpdatedAtProvider = c.UpdatedAt
				prev.ObservedAt = now
				if err := fbStore.UpsertPRConversationFeedback(ctx, prev); err != nil {
					return false, err
				}
				o.logger.Info("scm observer: conversation feedback invalidated by edit",
					"event", "ignored",
					"pr", prURL,
					"session", s.session.ID,
					"comment_id", c.ID,
					"reason", parseReason,
				)
			}
			continue
		}

		// Record pre-cutoff historic commands as baseline BEFORE checking
		// trustedAuthors so expanding trustedAuthors later cannot replay
		// comments that predate rollout/enablement.
		if prev, exists := existingByID[c.ID]; exists {
			if prev.State == domain.PRConversationFeedbackBaseline || prev.State == domain.PRConversationFeedbackSuperseded {
				continue
			}
		} else if !cutoff.IsZero() && !c.CreatedAt.IsZero() && c.CreatedAt.UTC().Before(cutoff) {
			baseline := domain.PullRequestConversationFeedback{
				PRURL:             prURL,
				CommentID:         c.ID,
				SessionID:         s.session.ID,
				SourceType:        domain.PRCommentFeedbackSourceType,
				Author:            c.Author,
				Body:              parsed.Body,
				BodyHash:          parsed.BodyHash,
				URL:               c.URL,
				State:             domain.PRConversationFeedbackBaseline,
				AutoInjectReview:  false,
				CreatedAtProvider: c.CreatedAt,
				UpdatedAtProvider: c.UpdatedAt,
				ObservedAt:        now,
			}
			if err := fbStore.UpsertPRConversationFeedback(ctx, baseline); err != nil {
				return false, err
			}
			o.logger.Info("scm observer: conversation feedback ignored",
				"event", "ignored",
				"pr", prURL,
				"session", s.session.ID,
				"comment_id", c.ID,
				"author", c.Author,
				"reason", "baseline_historic",
			)
			continue
		}

		allowed, authReason := domain.AuthorizePRCommentFeedback(cfg, c.Author, c.IsBot)
		if !allowed {
			if prev, exists := existingByID[c.ID]; exists && prev.State == domain.PRConversationFeedbackActive {
				prev.State = domain.PRConversationFeedbackDeleted
				prev.UpdatedAtProvider = c.UpdatedAt
				prev.ObservedAt = now
				if err := fbStore.UpsertPRConversationFeedback(ctx, prev); err != nil {
					return false, err
				}
			}
			o.logger.Info("scm observer: conversation feedback ignored",
				"event", "ignored",
				"pr", prURL,
				"session", s.session.ID,
				"comment_id", c.ID,
				"author", c.Author,
				"is_bot", c.IsBot,
				"reason", authReason,
			)
			continue
		}

		activeCandidates = append(activeCandidates, domain.PullRequestConversationFeedback{
			PRURL:             prURL,
			CommentID:         c.ID,
			SessionID:         s.session.ID,
			SourceType:        domain.PRCommentFeedbackSourceType,
			Author:            c.Author,
			Body:              parsed.Body,
			BodyHash:          parsed.BodyHash,
			URL:               c.URL,
			State:             domain.PRConversationFeedbackActive,
			AutoInjectReview:  s.session.AutoInjectReview,
			CreatedAtProvider: c.CreatedAt,
			UpdatedAtProvider: c.UpdatedAt,
			ObservedAt:        now,
		})
	}

	// Mark any previously active command absent from the fetched comments as
	// deleted ONLY on a complete scan. On a partial scan (newest 1,000 comments),
	// preserve absent active rows in storage (do not falsely mark them deleted)
	// while keeping hasActive=false so unverified rows are not delivered blindly.
	if result.Partial {
		o.logger.Warn("scm observer: conversation comments listing is partial; preserving absent active rows without delivery",
			"pr", prURL,
			"session", s.session.ID,
			"fetched_count", len(comments),
		)
	} else {
		for _, prev := range existingRows {
			if prev.State != domain.PRConversationFeedbackActive || presentIDs[prev.CommentID] {
				continue
			}
			prev.State = domain.PRConversationFeedbackDeleted
			prev.ObservedAt = now
			if err := fbStore.UpsertPRConversationFeedback(ctx, prev); err != nil {
				return false, err
			}
			o.logger.Info("scm observer: conversation feedback deleted",
				"event", "ignored",
				"pr", prURL,
				"session", s.session.ID,
				"comment_id", prev.CommentID,
				"reason", "deleted",
			)
		}
	}

	// Supersession: when multiple active commands exist, keep only the newest
	// command active and mark older ones superseded.
	sort.Slice(activeCandidates, func(i, j int) bool {
		if !activeCandidates[i].CreatedAtProvider.Equal(activeCandidates[j].CreatedAtProvider) {
			return activeCandidates[i].CreatedAtProvider.Before(activeCandidates[j].CreatedAtProvider)
		}
		return activeCandidates[i].CommentID < activeCandidates[j].CommentID
	})

	hasActive := false
	for i := range activeCandidates {
		item := activeCandidates[i]
		if i < len(activeCandidates)-1 {
			item.State = domain.PRConversationFeedbackSuperseded
		} else {
			hasActive = true
		}
		prev, exists := existingByID[item.CommentID]
		if !exists || prev.State != item.State || prev.BodyHash != item.BodyHash || prev.AutoInjectReview != item.AutoInjectReview || prev.SessionID != item.SessionID {
			if err := fbStore.UpsertPRConversationFeedback(ctx, item); err != nil {
				return false, err
			}
			o.logger.Info("scm observer: conversation feedback observed",
				"event", "observed",
				"pr", prURL,
				"session", s.session.ID,
				"comment_id", item.CommentID,
				"author", item.Author,
				"body_hash", item.BodyHash,
				"state", item.State,
			)
		}
	}

	// On partial results with a newer authorized command, supersede any absent
	// stored active row that is older than the newest fetched command so only
	// the newest command remains active.
	if result.Partial && len(activeCandidates) > 0 {
		newest := activeCandidates[len(activeCandidates)-1]
		for _, prev := range existingRows {
			if prev.State != domain.PRConversationFeedbackActive || presentIDs[prev.CommentID] {
				continue
			}
			older := prev.CreatedAtProvider.Before(newest.CreatedAtProvider) ||
				(prev.CreatedAtProvider.Equal(newest.CreatedAtProvider) && prev.CommentID < newest.CommentID)
			if older {
				prev.State = domain.PRConversationFeedbackSuperseded
				prev.ObservedAt = now
				if err := fbStore.UpsertPRConversationFeedback(ctx, prev); err != nil {
					return false, err
				}
				o.logger.Info("scm observer: conversation feedback superseded across partial window",
					"event", "ignored",
					"pr", prURL,
					"session", s.session.ID,
					"comment_id", prev.CommentID,
					"superseded_by", newest.CommentID,
					"reason", "superseded",
				)
			}
		}
	}

	if !initialized && !result.Partial {
		if err := fbStore.MarkPRConversationFeedbackInitialized(ctx, prURL, now); err != nil {
			return false, err
		}
	}

	return hasActive, nil
}
