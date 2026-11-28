package scm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type memConversationFeedbackStore struct {
	initialized       map[string]time.Time
	rows              map[string]map[string]domain.PullRequestConversationFeedback
	feedbackListCalls int
}

type conversationFeedbackTestProvider struct {
	Provider
	result ports.SCMConversationCommentsResult
	err    error
	calls  int
}

func (p *conversationFeedbackTestProvider) FetchConversationComments(context.Context, ports.SCMPRRef) (ports.SCMConversationCommentsResult, error) {
	p.calls++
	return p.result, p.err
}

type conversationFeedbackTestStore struct {
	Store
	*memConversationFeedbackStore
	project domain.ProjectRecord
}

func (s *conversationFeedbackTestStore) GetProject(context.Context, string) (domain.ProjectRecord, bool, error) {
	return s.project, true, nil
}

func (s *conversationFeedbackTestStore) ListChecks(context.Context, string) ([]domain.PullRequestCheck, error) {
	return nil, nil
}

func newMemConversationFeedbackStore() *memConversationFeedbackStore {
	return &memConversationFeedbackStore{
		initialized: map[string]time.Time{},
		rows:        map[string]map[string]domain.PullRequestConversationFeedback{},
	}
}

func (m *memConversationFeedbackStore) GetPRConversationFeedbackState(_ context.Context, prURL string) (time.Time, bool, error) {
	t, ok := m.initialized[prURL]
	return t, ok, nil
}

func (m *memConversationFeedbackStore) MarkPRConversationFeedbackInitialized(_ context.Context, prURL string, at time.Time) error {
	if _, ok := m.initialized[prURL]; !ok {
		m.initialized[prURL] = at
	}
	return nil
}

func (m *memConversationFeedbackStore) ListPRConversationFeedback(_ context.Context, prURL string) ([]domain.PullRequestConversationFeedback, error) {
	m.feedbackListCalls++
	byID := m.rows[prURL]
	out := make([]domain.PullRequestConversationFeedback, 0, len(byID))
	for _, r := range byID {
		out = append(out, r)
	}
	return out, nil
}

func (m *memConversationFeedbackStore) UpsertPRConversationFeedback(_ context.Context, item domain.PullRequestConversationFeedback) error {
	if m.rows[item.PRURL] == nil {
		m.rows[item.PRURL] = map[string]domain.PullRequestConversationFeedback{}
	}
	m.rows[item.PRURL][item.CommentID] = item
	return nil
}

func TestReconcileConversationFeedback_PartialWindowExcludesDeletedCommand(t *testing.T) {
	obs := New(nil, nil, nil, Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	fbStore := newMemConversationFeedbackStore()
	prURL := "https://github.com/wangyu0426/WebPoB/pull/9"
	now := time.Date(2026, 11, 28, 5, 0, 0, 0, time.UTC)

	subj := &subject{
		session: domain.SessionRecord{
			ID:               "webpob-1",
			AutoInjectReview: true,
			Activity:         domain.Activity{State: domain.ActivityWaitingInput, LastActivityAt: now},
		},
		known: domain.PullRequest{URL: prURL, Number: 9},
		hasPR: true,
	}
	cfg := domain.PRCommentFeedbackConfig{
		Enabled:        true,
		TrustedAuthors: []string{"wangyu0426"},
	}

	// 1. Busy worker: authorized command is observed and persisted as active.
	hasActive, err := obs.reconcileConversationFeedback(context.Background(), fbStore, subj, cfg, ports.SCMConversationCommentsResult{
		Comments: []ports.SCMConversationCommentObservation{{
			ID:        "IC_old_cmd",
			Author:    "wangyu0426",
			Body:      "/ao review-feedback Fix edge case",
			CreatedAt: now.Add(-10 * time.Minute),
		}},
		Partial: false,
	}, now)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !hasActive {
		t.Fatal("expected hasActive=true after observing authorized command")
	}

	// 2. Partial window (newest 1,000 comments) excludes the deleted command with no newer command.
	subj.session.Activity.State = domain.ActivityIdle
	hasActive, err = obs.reconcileConversationFeedback(context.Background(), fbStore, subj, cfg, ports.SCMConversationCommentsResult{
		Comments: []ports.SCMConversationCommentObservation{{
			ID:        "IC_newer_unmarked",
			Author:    "wangyu0426",
			Body:      "Unmarked chatter",
			CreatedAt: now.Add(-1 * time.Minute),
		}},
		Partial: true,
	}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("partial reconcile: %v", err)
	}
	if hasActive {
		t.Fatal("expected hasActive=false when partial window excludes deleted command")
	}
	rows, err := fbStore.ListPRConversationFeedback(context.Background(), prURL)
	if err != nil {
		t.Fatalf("ListPRConversationFeedback: %v", err)
	}
	if len(rows) != 1 || rows[0].State != domain.PRConversationFeedbackActive || !rows[0].Unverified {
		t.Fatalf("expected IC_old_cmd to be preserved but marked unverified on partial scan, got %+v", rows)
	}
}

func TestRefreshConversationFeedback_PartialScanBlocksCachedDelivery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 5, 0, 0, 0, time.UTC)
	prURL := "https://github.com/wangyu0426/WebPoB/pull/9"
	repo := ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "wangyu0426", Name: "WebPoB", Repo: "wangyu0426/WebPoB"}
	subj := &subject{
		session: domain.SessionRecord{ID: "webpob-1", ProjectID: "project-1", AutoInjectReview: true},
		repo:    repo,
		known:   domain.PullRequest{URL: prURL, Number: 9},
		hasPR:   true,
	}
	pkey := keyForTrackedPR(subj.known, repo)
	fbStore := newMemConversationFeedbackStore()
	if err := fbStore.UpsertPRConversationFeedback(ctx, domain.PullRequestConversationFeedback{
		PRURL:             prURL,
		CommentID:         "IC_old_cmd",
		SessionID:         subj.session.ID,
		Author:            "wangyu0426",
		Body:              "Fix edge case",
		BodyHash:          "old-hash",
		State:             domain.PRConversationFeedbackActive,
		AutoInjectReview:  true,
		CreatedAtProvider: now.Add(-time.Hour),
		ObservedAt:        now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed active feedback: %v", err)
	}
	provider := &conversationFeedbackTestProvider{result: ports.SCMConversationCommentsResult{
		Comments: []ports.SCMConversationCommentObservation{{
			ID:        "IC_newer_unmarked",
			Author:    "wangyu0426",
			Body:      "Unmarked chatter",
			CreatedAt: now.Add(-time.Minute),
		}},
		Partial: true,
	}}
	store := &conversationFeedbackTestStore{
		memConversationFeedbackStore: fbStore,
		project: domain.ProjectRecord{ID: "project-1", Config: domain.ProjectConfig{PRCommentFeedback: domain.PRCommentFeedbackConfig{
			Enabled:        true,
			TrustedAuthors: []string{"wangyu0426"},
		}}},
	}
	observer := New(provider, store, nil, Config{
		Clock:          func() time.Time { return now },
		ReviewInterval: 5 * time.Minute,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	subjects := map[string]*subject{"webpob-1": subj}
	subjectsByPR := map[string]*subject{pkey: subj}
	forced := map[string]bool{}
	localOnly := map[string]bool{}

	// A partial comment scan omits the older persisted command and must make it
	// ineligible until a complete scan or a visible command verifies it.
	observations := map[string]ports.SCMObservation{pkey: {
		Fetched:  true,
		Provider: "github",
		Host:     "github.com",
		Repo:     repo.Repo,
		PR:       ports.SCMPRObservation{URL: prURL, Number: 9},
	}}
	active := observer.refreshConversationFeedback(ctx, subjects, observations, subjectsByPR, forced, localOnly, now)
	if active[pkey] {
		t.Fatal("partial scan made omitted persisted feedback eligible for delivery")
	}
	if !observations[pkey].ConversationFeedbackUnverified {
		t.Fatal("partial scan did not mark the current PR observation unverified")
	}
	rows, err := fbStore.ListPRConversationFeedback(ctx, prURL)
	if err != nil {
		t.Fatalf("list persisted feedback after partial scan: %v", err)
	}
	if len(rows) != 1 || !rows[0].Unverified {
		t.Fatalf("partial scan did not persist unverified state: %+v", rows)
	}

	// The PR can be freshly observed while its comment scan remains a cache hit.
	// The new observation starts with the zero-value flag; persisted verification
	// state must still suppress the old command without another comment fetch.
	observations[pkey] = ports.SCMObservation{
		Fetched:  true,
		Provider: "github",
		Host:     "github.com",
		Repo:     repo.Repo,
		PR:       ports.SCMPRObservation{URL: prURL, Number: 9},
	}
	active = observer.refreshConversationFeedback(ctx, subjects, observations, subjectsByPR, forced, localOnly, now.Add(time.Minute))
	if active[pkey] {
		t.Fatal("cached partial-scan state re-enabled delivery of omitted persisted feedback")
	}
	if !observations[pkey].ConversationFeedbackUnverified {
		t.Fatal("cached PR observation lost the persisted unverified state")
	}
	if provider.calls != 1 {
		t.Fatalf("comment fetches = %d, want one partial fetch followed by a cache hit", provider.calls)
	}

	// Seeing the command again verifies that row and makes it deliverable.
	provider.result = ports.SCMConversationCommentsResult{
		Comments: []ports.SCMConversationCommentObservation{{
			ID:        "IC_old_cmd",
			Author:    "wangyu0426",
			Body:      "/ao review-feedback Fix edge case",
			CreatedAt: now.Add(-time.Hour),
		}},
		Partial: true,
	}
	observations[pkey] = ports.SCMObservation{
		Fetched:  true,
		Provider: "github",
		Host:     "github.com",
		Repo:     repo.Repo,
		PR:       ports.SCMPRObservation{URL: prURL, Number: 9},
	}
	active = observer.refreshConversationFeedback(ctx, subjects, observations, subjectsByPR, forced, localOnly, now.Add(6*time.Minute))
	if !active[pkey] {
		t.Fatal("visible command did not re-enable verified feedback delivery")
	}
	if observations[pkey].ConversationFeedbackUnverified {
		t.Fatal("visible command left the PR observation unverified")
	}
	rows, err = fbStore.ListPRConversationFeedback(ctx, prURL)
	if err != nil {
		t.Fatalf("list persisted feedback after visible command: %v", err)
	}
	if len(rows) != 1 || rows[0].Unverified {
		t.Fatalf("visible command did not clear persisted unverified state: %+v", rows)
	}
	if provider.calls != 2 {
		t.Fatalf("comment fetches = %d, want second fetch after review interval", provider.calls)
	}
}

func TestRefreshConversationFeedback_TerminalResponseSuppressesVerifiedFeedback(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 8, 0, 0, 0, time.UTC)
	prURL := "https://github.com/wangyu0426/WebPoB/pull/9"
	repo := ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "wangyu0426", Name: "WebPoB", Repo: "wangyu0426/WebPoB"}
	tests := []struct {
		name   string
		merged bool
		closed bool
	}{
		{name: "merged response", merged: true},
		{name: "closed response", closed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			makeSubject := func(id string) *subject {
				return &subject{
					session: domain.SessionRecord{ID: domain.SessionID(id), ProjectID: "project-1", AutoInjectReview: true},
					repo:    repo,
					known:   domain.PullRequest{URL: prURL, Number: 9},
					hasPR:   true,
				}
			}
			subject1 := makeSubject("webpob-1")
			subject2 := makeSubject("webpob-2")
			pkey := keyForTrackedPR(subject1.known, repo)
			fbStore := newMemConversationFeedbackStore()
			if err := fbStore.UpsertPRConversationFeedback(ctx, domain.PullRequestConversationFeedback{
				PRURL:             prURL,
				CommentID:         "IC_verified_cmd",
				SessionID:         subject1.session.ID,
				Author:            "wangyu0426",
				Body:              "Fix edge case",
				BodyHash:          "verified-hash",
				State:             domain.PRConversationFeedbackActive,
				AutoInjectReview:  true,
				CreatedAtProvider: now.Add(-time.Hour),
				ObservedAt:        now.Add(-time.Hour),
			}); err != nil {
				t.Fatalf("seed active feedback: %v", err)
			}
			provider := &conversationFeedbackTestProvider{result: ports.SCMConversationCommentsResult{
				PRMerged: tt.merged,
				PRClosed: tt.closed,
			}}
			store := &conversationFeedbackTestStore{
				memConversationFeedbackStore: fbStore,
				project: domain.ProjectRecord{ID: "project-1", Config: domain.ProjectConfig{PRCommentFeedback: domain.PRCommentFeedbackConfig{
					Enabled:        true,
					TrustedAuthors: []string{"wangyu0426"},
				}}},
			}
			observer := New(provider, store, nil, Config{
				Clock:          func() time.Time { return now },
				ReviewInterval: 5 * time.Minute,
				Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			subjects := map[string]*subject{"webpob-1": subject1, "webpob-2": subject2}
			observations := map[string]ports.SCMObservation{pkey: {
				Fetched:  true,
				Provider: "github",
				Host:     "github.com",
				Repo:     repo.Repo,
				PR:       ports.SCMPRObservation{URL: prURL, Number: 9, Title: "changed since prior poll"},
			}}
			active := observer.refreshConversationFeedback(ctx, subjects, observations, map[string]*subject{pkey: subject1}, map[string]bool{}, map[string]bool{}, now)
			if active[pkey] {
				t.Fatal("terminal conversation response left verified feedback eligible for delivery")
			}
			got := observations[pkey]
			if got.PR.Merged != tt.merged || got.PR.Closed != tt.closed {
				t.Fatalf("PR terminal state = merged:%v closed:%v, want merged:%v closed:%v", got.PR.Merged, got.PR.Closed, tt.merged, tt.closed)
			}
			if provider.calls != 1 {
				t.Fatalf("conversation fetches = %d, want one before the shared observation becomes terminal", provider.calls)
			}
			if fbStore.feedbackListCalls != 0 {
				t.Fatalf("persisted feedback status reads = %d, want none after the terminal response", fbStore.feedbackListCalls)
			}
			row := fbStore.rows[prURL]["IC_verified_cmd"]
			if row.State != domain.PRConversationFeedbackActive || row.Unverified {
				t.Fatalf("terminal response changed the verified active row: %+v", row)
			}
		})
	}
}

func TestRefreshConversationFeedback_ReviewOnlyObservationRequiresFreshOpenEvidence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	prURL := "https://github.com/wangyu0426/WebPoB/pull/9"
	repo := ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "wangyu0426", Name: "WebPoB", Repo: "wangyu0426/WebPoB"}
	tests := []struct {
		name           string
		providerErr    error
		skipRefresh    bool
		wantFetchCalls int
	}{
		{name: "failed comment refresh", providerErr: errors.New("temporary fetch failure"), wantFetchCalls: 1},
		{name: "cadence cache hit", skipRefresh: true, wantFetchCalls: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subj := &subject{
				session: domain.SessionRecord{ID: "webpob-1", ProjectID: "project-1", AutoInjectReview: true},
				repo:    repo,
				known:   domain.PullRequest{URL: prURL, Number: 9},
				hasPR:   true,
			}
			pkey := keyForTrackedPR(subj.known, repo)
			fbStore := newMemConversationFeedbackStore()
			if err := fbStore.UpsertPRConversationFeedback(ctx, domain.PullRequestConversationFeedback{
				PRURL:             prURL,
				CommentID:         "IC_verified_cmd",
				SessionID:         subj.session.ID,
				Author:            "wangyu0426",
				Body:              "Fix edge case",
				BodyHash:          "verified-hash",
				State:             domain.PRConversationFeedbackActive,
				AutoInjectReview:  true,
				CreatedAtProvider: now.Add(-time.Hour),
				ObservedAt:        now.Add(-time.Hour),
			}); err != nil {
				t.Fatalf("seed active feedback: %v", err)
			}
			provider := &conversationFeedbackTestProvider{err: tt.providerErr}
			store := &conversationFeedbackTestStore{
				memConversationFeedbackStore: fbStore,
				project: domain.ProjectRecord{ID: "project-1", Config: domain.ProjectConfig{PRCommentFeedback: domain.PRCommentFeedbackConfig{
					Enabled:        true,
					TrustedAuthors: []string{"wangyu0426"},
				}}},
			}
			observer := New(provider, store, nil, Config{
				Clock:          func() time.Time { return now },
				ReviewInterval: 5 * time.Minute,
				Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			if tt.skipRefresh {
				observer.Cache.LastConversationPollAt[pkey] = now
			}
			observations := map[string]ports.SCMObservation{pkey: observationFromLocal(repo, subj.known, nil)}
			active := observer.refreshConversationFeedback(
				ctx,
				map[string]*subject{"webpob-1": subj},
				observations,
				map[string]*subject{pkey: subj},
				map[string]bool{},
				map[string]bool{pkey: true},
				now,
			)
			if active[pkey] {
				t.Fatal("review-only OPEN snapshot made persisted feedback eligible without fresh open evidence")
			}
			if !observations[pkey].ConversationFeedbackUnverified {
				t.Fatal("review-only OPEN snapshot did not suppress conversation feedback on the dispatched observation")
			}
			if provider.calls != tt.wantFetchCalls {
				t.Fatalf("conversation fetches = %d, want %d", provider.calls, tt.wantFetchCalls)
			}
		})
	}
}

func TestRefreshConversationFeedback_TerminalResultWithoutMainObservationSuppressesNextReviewOnlyPoll(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	prURL := "https://github.com/wangyu0426/WebPoB/pull/9"
	repo := ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "wangyu0426", Name: "WebPoB", Repo: "wangyu0426/WebPoB"}
	subj := &subject{
		session: domain.SessionRecord{ID: "webpob-1", ProjectID: "project-1", AutoInjectReview: true},
		repo:    repo,
		known:   domain.PullRequest{URL: prURL, Number: 9},
		hasPR:   true,
	}
	pkey := keyForTrackedPR(subj.known, repo)
	fbStore := newMemConversationFeedbackStore()
	if err := fbStore.UpsertPRConversationFeedback(ctx, domain.PullRequestConversationFeedback{
		PRURL:             prURL,
		CommentID:         "IC_verified_cmd",
		SessionID:         subj.session.ID,
		Author:            "wangyu0426",
		Body:              "Fix edge case",
		BodyHash:          "verified-hash",
		State:             domain.PRConversationFeedbackActive,
		AutoInjectReview:  true,
		CreatedAtProvider: now.Add(-time.Hour),
		ObservedAt:        now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed active feedback: %v", err)
	}
	provider := &conversationFeedbackTestProvider{result: ports.SCMConversationCommentsResult{PRClosed: true}}
	store := &conversationFeedbackTestStore{
		memConversationFeedbackStore: fbStore,
		project: domain.ProjectRecord{ID: "project-1", Config: domain.ProjectConfig{PRCommentFeedback: domain.PRCommentFeedbackConfig{
			Enabled:        true,
			TrustedAuthors: []string{"wangyu0426"},
		}}},
	}
	observer := New(provider, store, nil, Config{
		Clock:          func() time.Time { return now },
		ReviewInterval: 5 * time.Minute,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	subjects := map[string]*subject{"webpob-1": subj}
	subjectsByPR := map[string]*subject{pkey: subj}
	forced := map[string]bool{}
	localOnly := map[string]bool{}

	// A terminal comment result without a main observation must still cache the
	// poll, then suppress delivery if the next poll has only a cached OPEN row.
	observations := map[string]ports.SCMObservation{}
	active := observer.refreshConversationFeedback(ctx, subjects, observations, subjectsByPR, forced, localOnly, now)
	if active[pkey] {
		t.Fatal("terminal result without a main observation made feedback eligible")
	}
	if provider.calls != 1 {
		t.Fatalf("conversation fetches after terminal result = %d, want 1", provider.calls)
	}

	observations[pkey] = observationFromLocal(repo, subj.known, nil)
	localOnly[pkey] = true
	active = observer.refreshConversationFeedback(ctx, subjects, observations, subjectsByPR, forced, localOnly, now.Add(time.Minute))
	if active[pkey] {
		t.Fatal("subsequent review-only OPEN snapshot re-enabled cached verified feedback")
	}
	if !observations[pkey].ConversationFeedbackUnverified {
		t.Fatal("subsequent review-only observation did not suppress feedback delivery")
	}
	if provider.calls != 1 {
		t.Fatalf("conversation fetches = %d, want terminal fetch followed by cadence cache hit", provider.calls)
	}
}

func TestRefreshConversationFeedback_UnsupportedProviderSuppressesLocalOnlyObservation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	prURL := "https://github.com/wangyu0426/WebPoB/pull/9"
	repo := ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "wangyu0426", Name: "WebPoB", Repo: "wangyu0426/WebPoB"}
	subj := &subject{
		session: domain.SessionRecord{ID: "webpob-1", ProjectID: "project-1", AutoInjectReview: true},
		repo:    repo,
		known:   domain.PullRequest{URL: prURL, Number: 9},
		hasPR:   true,
	}
	pkey := keyForTrackedPR(subj.known, repo)
	fbStore := newMemConversationFeedbackStore()
	if err := fbStore.UpsertPRConversationFeedback(ctx, domain.PullRequestConversationFeedback{
		PRURL:             prURL,
		CommentID:         "IC_verified_cmd",
		SessionID:         subj.session.ID,
		Author:            "wangyu0426",
		Body:              "Fix edge case",
		BodyHash:          "verified-hash",
		State:             domain.PRConversationFeedbackActive,
		AutoInjectReview:  true,
		CreatedAtProvider: now.Add(-time.Hour),
		ObservedAt:        now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed active feedback: %v", err)
	}
	store := &conversationFeedbackTestStore{memConversationFeedbackStore: fbStore}
	observer := New(nil, store, nil, Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	observations := map[string]ports.SCMObservation{pkey: observationFromLocal(repo, subj.known, nil)}
	active := observer.refreshConversationFeedback(
		ctx,
		map[string]*subject{"webpob-1": subj},
		observations,
		map[string]*subject{pkey: subj},
		map[string]bool{},
		map[string]bool{pkey: true},
		now,
	)
	if active[pkey] {
		t.Fatal("unsupported provider made persisted feedback eligible from a review-only OPEN snapshot")
	}
	if !observations[pkey].ConversationFeedbackUnverified {
		t.Fatal("unsupported provider did not suppress feedback on the local-only observation")
	}
}
