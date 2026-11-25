package scm

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestNormalizedRemoteBranch(t *testing.T) {
	cases := []struct {
		name  string
		state *domain.SessionBranchState
		want  string
	}{
		{name: "nil state", state: nil, want: ""},
		{name: "empty", state: &domain.SessionBranchState{}, want: ""},
		{name: "whitespace", state: &domain.SessionBranchState{RemoteBranch: "   "}, want: ""},
		{name: "origin prefix", state: &domain.SessionBranchState{RemoteBranch: "origin/feat/issue-16"}, want: "feat/issue-16"},
		{name: "origin only", state: &domain.SessionBranchState{RemoteBranch: "origin/"}, want: ""},
		{name: "refs remotes origin", state: &domain.SessionBranchState{RemoteBranch: "refs/remotes/origin/feat/issue-16"}, want: "feat/issue-16"},
		{name: "refs remotes upstream", state: &domain.SessionBranchState{RemoteBranch: "refs/remotes/upstream/fix/bug"}, want: "fix/bug"},
		{name: "refs remotes without branch", state: &domain.SessionBranchState{RemoteBranch: "refs/remotes/origin"}, want: ""},
		{name: "refs heads", state: &domain.SessionBranchState{RemoteBranch: "refs/heads/feat/issue-16"}, want: "feat/issue-16"},
		{name: "bare branch", state: &domain.SessionBranchState{RemoteBranch: "feat/issue-16"}, want: "feat/issue-16"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizedRemoteBranch(tc.state); got != tc.want {
				t.Fatalf("normalizedRemoteBranch(%+v) = %q; want %q", tc.state, got, tc.want)
			}
		})
	}
}

func TestPoll_DiscoversPRFromPersistedRemoteBranch(t *testing.T) {
	store := testStoreWithSession()
	store.sessions[0].Metadata.Branch = "ao/work-on-tracker-16/root"
	store.sessions[0].BranchState = &domain.SessionBranchState{
		Branch:       "feat/issue-16",
		RemoteBranch: "origin/feat/issue-16",
	}

	prObs := testObs(145)
	prObs.PR.SourceBranch = "feat/issue-16"
	prObs.PR.HeadRepo = "o/r"
	prObs.PR.BaseRepo = "o/r"
	prObs.PR.Author = "alice"
	prObs.PR.ProviderID = "PR_kwDO_145"

	provider := &fakeProvider{
		identity:     ports.SCMIdentity{Login: "alice", Human: true},
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v1"}},
		openPRs:      map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {prObs.PR}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 145): prObs},
	}
	lc := &fakeLifecycle{}
	now := time.Unix(1_800_000_000, 0).UTC()
	obs := New(provider, store, lc, Config{
		Logger:                 quietSlog(),
		Clock:                  func() time.Time { return now },
		ScopedIdentityResolver: provider,
	})

	if err := obs.Poll(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if len(lc.observed) != 1 {
		t.Fatalf("lifecycle notifications = %d; want 1", len(lc.observed))
	}
	if lc.observed[0].PR.Number != 145 {
		t.Fatalf("unexpected lifecycle observation: %+v", lc.observed[0])
	}
	if len(store.writes) == 0 {
		t.Fatal("expected PR #145 to be written to store")
	}
	lastPR := store.writes[len(store.writes)-1].pr
	if lastPR.SessionID != "p-1" || lastPR.Number != 145 || lastPR.SourceBranch != "feat/issue-16" {
		t.Fatalf("persisted PR = %+v; want PR #145 for p-1", lastPR)
	}

	// Repeated polling must be idempotent: no duplicate association or lifecycle event.
	store.prs["p-1"] = []domain.PullRequest{lastPR}
	writesBefore := len(store.writes)
	now = now.Add(30 * time.Second)
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if len(lc.observed) != 1 {
		t.Fatalf("lifecycle notifications after repeat poll = %d; want 1", len(lc.observed))
	}
	if len(store.writes) != writesBefore {
		t.Fatalf("store writes after repeat poll = %d; want %d", len(store.writes), writesBefore)
	}
}

func TestPoll_RemoteBranchDiscoveryEdgeCases(t *testing.T) {
	t.Run("legacy metadata match when remoteBranch absent or stale", func(t *testing.T) {
		for _, state := range []*domain.SessionBranchState{
			nil,
			{RemoteBranch: ""},
			{RemoteBranch: "origin/stale/other-branch"},
		} {
			store := testStoreWithSession()
			store.sessions[0].Metadata.Branch = "ao/work-on-tracker-17/root"
			store.sessions[0].BranchState = state

			prObs := testObs(146)
			prObs.PR.SourceBranch = "ao/work-on-tracker-17/root"
			prObs.PR.HeadRepo = "o/r"
			prObs.PR.Author = "alice"
			provider := &fakeProvider{
				identity:     ports.SCMIdentity{Login: "alice", Human: true},
				repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v1"}},
				openPRs:      map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {prObs.PR}},
				observations: map[string]ports.SCMObservation{prKey(testRepo, 146): prObs},
			}
			lc := &fakeLifecycle{}
			obs := New(provider, store, lc, Config{Logger: quietSlog(), Clock: func() time.Time { return time.Unix(1, 0).UTC() }, ScopedIdentityResolver: provider})
			if err := obs.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(lc.observed) != 1 || lc.observed[0].PR.Number != 146 {
				t.Fatalf("legacy metadata match failed for state %+v: observed=%+v", state, lc.observed)
			}
		}
	})

	t.Run("absent or stale remoteBranch does not falsely associate", func(t *testing.T) {
		store := testStoreWithSession()
		store.sessions[0].Metadata.Branch = "ao/work-on-tracker-18/root"
		store.sessions[0].BranchState = &domain.SessionBranchState{RemoteBranch: "origin/feat/different-branch"}

		prObs := testObs(147)
		prObs.PR.SourceBranch = "feat/issue-18"
		prObs.PR.HeadRepo = "o/r"
		prObs.PR.Author = "alice"
		provider := &fakeProvider{
			identity:     ports.SCMIdentity{Login: "alice", Human: true},
			repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v1"}},
			openPRs:      map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {prObs.PR}},
			observations: map[string]ports.SCMObservation{prKey(testRepo, 147): prObs},
		}
		lc := &fakeLifecycle{}
		obs := New(provider, store, lc, Config{Logger: quietSlog(), Clock: func() time.Time { return time.Unix(1, 0).UTC() }, ScopedIdentityResolver: provider})
		if err := obs.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(lc.observed) != 0 || len(store.prs["p-1"]) != 0 {
			t.Fatalf("stale remoteBranch falsely associated PR: observed=%+v prs=%+v", lc.observed, store.prs["p-1"])
		}
	})

	t.Run("cross-repo foreign fork head is rejected even when remoteBranch matches", func(t *testing.T) {
		store := testStoreWithSession()
		store.sessions[0].Metadata.Branch = "ao/work-on-tracker-19/root"
		store.sessions[0].BranchState = &domain.SessionBranchState{RemoteBranch: "origin/feat/issue-19"}

		prObs := testObs(148)
		prObs.PR.SourceBranch = "feat/issue-19"
		prObs.PR.HeadRepo = "stranger/r"
		prObs.PR.BaseRepo = "o/r"
		prObs.PR.Author = "alice"
		provider := &fakeProvider{
			identity:     ports.SCMIdentity{Login: "alice", Human: true},
			repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v1"}},
			openPRs:      map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {prObs.PR}},
			observations: map[string]ports.SCMObservation{prKey(testRepo, 148): prObs},
		}
		lc := &fakeLifecycle{}
		obs := New(provider, store, lc, Config{Logger: quietSlog(), Clock: func() time.Time { return time.Unix(1, 0).UTC() }, ScopedIdentityResolver: provider})
		if err := obs.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(lc.observed) != 0 || len(store.prs["p-1"]) != 0 {
			t.Fatalf("foreign fork PR was falsely associated: observed=%+v prs=%+v", lc.observed, store.prs["p-1"])
		}
	})

	t.Run("conflicting and ambiguous candidates fail closed", func(t *testing.T) {
		cases := []struct {
			name string
			s1   domain.SessionRecord
			s2   domain.SessionRecord
		}{
			{
				name: "two sessions with same remoteBranch",
				s1: domain.SessionRecord{
					ID: "p-1", ProjectID: "p",
					Metadata:    domain.SessionMetadata{Branch: "ao/work-1/root"},
					BranchState: &domain.SessionBranchState{RemoteBranch: "origin/feat/shared"},
				},
				s2: domain.SessionRecord{
					ID: "p-2", ProjectID: "p",
					Metadata:    domain.SessionMetadata{Branch: "ao/work-2/root"},
					BranchState: &domain.SessionBranchState{RemoteBranch: "origin/feat/shared"},
				},
			},
			{
				name: "metadata branch conflicts with another session remoteBranch",
				s1: domain.SessionRecord{
					ID: "p-1", ProjectID: "p",
					Metadata: domain.SessionMetadata{Branch: "feat/shared"},
				},
				s2: domain.SessionRecord{
					ID: "p-2", ProjectID: "p",
					Metadata:    domain.SessionMetadata{Branch: "ao/work-2/root"},
					BranchState: &domain.SessionBranchState{RemoteBranch: "origin/feat/shared"},
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				store := testStoreWithSession()
				store.sessions = []domain.SessionRecord{tc.s1, tc.s2}

				prObs := testObs(149)
				prObs.PR.SourceBranch = "feat/shared"
				prObs.PR.HeadRepo = "o/r"
				prObs.PR.Author = "alice"
				provider := &fakeProvider{
					identity:     ports.SCMIdentity{Login: "alice", Human: true},
					repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v1"}},
					openPRs:      map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {prObs.PR}},
					observations: map[string]ports.SCMObservation{prKey(testRepo, 149): prObs},
				}
				lc := &fakeLifecycle{}
				obs := New(provider, store, lc, Config{Logger: quietSlog(), Clock: func() time.Time { return time.Unix(1, 0).UTC() }, ScopedIdentityResolver: provider})
				if err := obs.Poll(context.Background()); err != nil {
					t.Fatal(err)
				}
				if len(lc.observed) != 0 || len(store.prs["p-1"]) != 0 || len(store.prs["p-2"]) != 0 {
					t.Fatalf("ambiguous match should fail closed: observed=%+v p1=%+v p2=%+v", lc.observed, store.prs["p-1"], store.prs["p-2"])
				}
			})
		}
	})
}

type mutatingBranchStates struct {
	fn func()
}

func (m *mutatingBranchStates) ReconcileBranchStates(context.Context) error {
	if m.fn != nil {
		m.fn()
	}
	return nil
}

func TestPoll_RefreshedBranchStateDiscoversPR(t *testing.T) {
	store := testStoreWithSession()
	store.sessions[0].Metadata.Branch = "ao/work-on-tracker-20/root"
	store.sessions[0].BranchState = nil

	prObs := testObs(150)
	prObs.PR.SourceBranch = "feat/issue-20"
	prObs.PR.HeadRepo = "o/r"
	prObs.PR.Author = "alice"
	provider := &fakeProvider{
		identity:     ports.SCMIdentity{Login: "alice", Human: true},
		repoGuards:   map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v1"}},
		openPRs:      map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {prObs.PR}},
		observations: map[string]ports.SCMObservation{prKey(testRepo, 150): prObs},
	}
	lc := &fakeLifecycle{}
	reconciler := &mutatingBranchStates{
		fn: func() {
			store.sessions[0].BranchState = &domain.SessionBranchState{
				Branch:       "feat/issue-20",
				RemoteBranch: "origin/feat/issue-20",
			}
		},
	}
	obs := New(provider, store, lc, Config{
		Logger:                 quietSlog(),
		Clock:                  func() time.Time { return time.Unix(1, 0).UTC() },
		ScopedIdentityResolver: provider,
		BranchStates:           reconciler,
	})
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(lc.observed) != 1 || lc.observed[0].PR.Number != 150 {
		t.Fatalf("refreshed branch state did not discover PR: observed=%+v", lc.observed)
	}
}

func TestPoll_RestartReconciliationAndExplicitClaimPrecedence(t *testing.T) {
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID:            "p",
		Path:          t.TempDir(),
		RepoOriginURL: "https://github.com/o/r.git",
		RegisteredAt:  now,
	}); err != nil {
		t.Fatal(err)
	}

	// Session 1: affected session whose metadata branch differs from its pushed remoteBranch.
	sess1, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p",
		Kind:      domain.KindWorker,
		Harness:   domain.HarnessFake,
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		Metadata:  domain.SessionMetadata{Branch: "ao/work-on-tracker-16/root", WorkspacePath: t.TempDir()},
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetSessionBranchState(ctx, sess1.ID, domain.SessionBranchState{
		Branch:       "feat/issue-16",
		RemoteBranch: "origin/feat/issue-16",
	}); err != nil {
		t.Fatal(err)
	}

	// Session 2: explicitly claims PR #144 even though Session 3 has remoteBranch matching PR #144.
	sess2, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p",
		Kind:      domain.KindWorker,
		Harness:   domain.HarnessFake,
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		Metadata:  domain.SessionMetadata{Branch: "ao/work-on-tracker-17/root", WorkspacePath: t.TempDir()},
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess3, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p",
		Kind:      domain.KindWorker,
		Harness:   domain.HarnessFake,
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		Metadata:  domain.SessionMetadata{Branch: "ao/work-on-tracker-18/root", WorkspacePath: t.TempDir()},
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetSessionBranchState(ctx, sess3.ID, domain.SessionBranchState{
		Branch:       "feat/claimed-144",
		RemoteBranch: "origin/feat/claimed-144",
	}); err != nil {
		t.Fatal(err)
	}
	claimedPR := domain.PullRequest{
		URL:          "https://github.com/o/r/pull/144",
		SessionID:    sess2.ID,
		Number:       144,
		Provider:     "github",
		Host:         "github.com",
		Repo:         "o/r",
		ProviderID:   "PR_kwDO_144",
		SourceBranch: "feat/claimed-144",
		Author:       "alice",
		UpdatedAt:    now,
	}
	if _, err := store.ClaimPR(ctx, claimedPR, nil, nil, nil, nil, ports.ReviewWritePreserve, false); err != nil {
		t.Fatal(err)
	}

	obs145 := testObs(145)
	obs145.PR.SourceBranch = "feat/issue-16"
	obs145.PR.HeadRepo = "o/r"
	obs145.PR.BaseRepo = "o/r"
	obs145.PR.Author = "alice"
	obs145.PR.ProviderID = "PR_kwDO_145"

	obs144 := testObs(144)
	obs144.PR.SourceBranch = "feat/claimed-144"
	obs144.PR.HeadRepo = "o/r"
	obs144.PR.BaseRepo = "o/r"
	obs144.PR.Author = "alice"
	obs144.PR.ProviderID = "PR_kwDO_144"

	newProvider := func() *fakeProvider {
		return &fakeProvider{
			identity:   ports.SCMIdentity{Login: "alice", Human: true},
			repoGuards: map[string]ports.SCMGuardResult{prKey(testRepo, 0): {ETag: "v1"}},
			openPRs:    map[string][]ports.SCMPRObservation{prKey(testRepo, 0): {obs145.PR, obs144.PR}},
			observations: map[string]ports.SCMObservation{
				prKey(testRepo, 145): obs145,
				prKey(testRepo, 144): obs144,
			},
		}
	}

	// First reconciliation (simulates startup after restart).
	lc1 := &fakeLifecycle{}
	p1 := newProvider()
	observer1 := New(p1, store, lc1, Config{
		Logger:                 quietSlog(),
		Clock:                  func() time.Time { return now },
		ScopedIdentityResolver: p1,
	})
	if err := observer1.Poll(ctx); err != nil {
		t.Fatalf("first reconciliation poll: %v", err)
	}

	got1, err := store.ListPRsBySession(ctx, sess1.ID)
	if err != nil || len(got1) != 1 || got1[0].Number != 145 {
		t.Fatalf("sess1 PRs = %+v, err=%v; want PR #145", got1, err)
	}
	got2, err := store.ListPRsBySession(ctx, sess2.ID)
	if err != nil || len(got2) != 1 || got2[0].Number != 144 {
		t.Fatalf("sess2 PRs = %+v, err=%v; want explicitly claimed PR #144", got2, err)
	}
	got3, err := store.ListPRsBySession(ctx, sess3.ID)
	if err != nil || len(got3) != 0 {
		t.Fatalf("sess3 stole claimed PR #144: %+v, err=%v", got3, err)
	}
	sess1Events := 0
	for _, ev := range lc1.observed {
		if ev.PR.Number == 145 {
			sess1Events++
		}
	}
	if sess1Events != 1 {
		t.Fatalf("sess1 lifecycle events on first reconciliation = %d; want 1", sess1Events)
	}

	// Second reconciliation with a brand new Observer (simulates another restart).
	now = now.Add(time.Minute)
	lc2 := &fakeLifecycle{}
	p2 := newProvider()
	observer2 := New(p2, store, lc2, Config{
		Logger:                 quietSlog(),
		Clock:                  func() time.Time { return now },
		ScopedIdentityResolver: p2,
	})
	if err := observer2.Poll(ctx); err != nil {
		t.Fatalf("second reconciliation poll: %v", err)
	}
	if len(lc2.observed) != 0 {
		t.Fatalf("restart reconciliation emitted duplicate lifecycle events: %+v", lc2.observed)
	}
	got1After, err := store.ListPRsBySession(ctx, sess1.ID)
	if err != nil || len(got1After) != 1 || got1After[0].Number != 145 {
		t.Fatalf("sess1 PRs after restart = %+v, err=%v; want single PR #145", got1After, err)
	}
	got3After, err := store.ListPRsBySession(ctx, sess3.ID)
	if err != nil || len(got3After) != 0 {
		t.Fatalf("sess3 stole PR #144 after restart: %+v, err=%v", got3After, err)
	}
}

func TestPoll_WorkspaceDoesNotLeakSessionRemoteBranchAcrossRepos(t *testing.T) {
	repoA := ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "o", Name: "repo-a", Repo: "o/repo-a"}
	repoB := ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "o", Name: "repo-b", Repo: "o/repo-b"}

	store := testStoreWithSession()
	store.sessions[0].Metadata.Branch = "ao/work-on-tracker-16/root"
	store.sessions[0].BranchState = &domain.SessionBranchState{
		Branch:       "feat/issue-16",
		RemoteBranch: "origin/feat/issue-16",
	}
	store.projects["p"] = domain.ProjectRecord{
		ID:   "p",
		Kind: domain.ProjectKindWorkspace,
	}
	store.workspaceRepos = map[string][]domain.WorkspaceRepoRecord{
		"p": {
			{Name: "repo-a", RelativePath: "repo-a", RepoOriginURL: "https://github.com/o/repo-a.git"},
			{Name: "repo-b", RelativePath: "repo-b", RepoOriginURL: "https://github.com/o/repo-b.git"},
		},
	}

	// Unrelated PR in repo-b with the same branch name as session's remoteBranch.
	prB := testObs(201)
	prB.Repo = "o/repo-b"
	prB.PR.URL = "https://github.com/o/repo-b/pull/201"
	prB.PR.HTMLURL = prB.PR.URL
	prB.PR.SourceBranch = "feat/issue-16"
	prB.PR.HeadRepo = "o/repo-b"
	prB.PR.BaseRepo = "o/repo-b"
	prB.PR.Author = "alice"

	// Legitimate PR in repo-a matching the session's metadata branch.
	prA := testObs(202)
	prA.Repo = "o/repo-a"
	prA.PR.URL = "https://github.com/o/repo-a/pull/202"
	prA.PR.HTMLURL = prA.PR.URL
	prA.PR.SourceBranch = "ao/work-on-tracker-16/root"
	prA.PR.HeadRepo = "o/repo-a"
	prA.PR.BaseRepo = "o/repo-a"
	prA.PR.Author = "alice"

	provider := &fakeProvider{
		identity: ports.SCMIdentity{Login: "alice", Human: true},
		repoGuards: map[string]ports.SCMGuardResult{
			prKey(repoA, 0): {ETag: "a-v1"},
			prKey(repoB, 0): {ETag: "b-v1"},
		},
		openPRs: map[string][]ports.SCMPRObservation{
			prKey(repoA, 0): {prA.PR},
			prKey(repoB, 0): {prB.PR},
		},
		observations: map[string]ports.SCMObservation{
			prKey(repoA, 202): prA,
			prKey(repoB, 201): prB,
		},
	}
	lc := &fakeLifecycle{}
	obs := New(provider, store, lc, Config{
		Logger:                 quietSlog(),
		Clock:                  func() time.Time { return time.Unix(1, 0).UTC() },
		ScopedIdentityResolver: provider,
	})
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(lc.observed) != 1 || lc.observed[0].PR.Number != 202 || lc.observed[0].Repo != "o/repo-a" {
		t.Fatalf("workspace leaked remoteBranch across repos: observed=%+v; want only repo-a PR #202", lc.observed)
	}
	for _, w := range store.writes {
		if w.pr.Repo == "o/repo-b" || w.pr.Number == 201 {
			t.Fatalf("unrelated workspace repo PR #201 was falsely associated: %+v", w.pr)
		}
	}
}
