package project_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/trackerintake"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

func openTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "ao.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

type fakeBootstrapTracker struct {
	issues       []domain.Issue
	preflightErr error
}

func (f *fakeBootstrapTracker) Get(_ context.Context, _ domain.TrackerID) (domain.Issue, error) {
	return domain.Issue{}, errors.New("unused")
}
func (f *fakeBootstrapTracker) List(_ context.Context, _ domain.TrackerRepo, _ domain.ListFilter) ([]domain.Issue, error) {
	return f.issues, nil
}
func (f *fakeBootstrapTracker) Preflight(_ context.Context) error { return f.preflightErr }

type fakeBootstrapSpawner struct {
	mu      sync.Mutex
	spawned []ports.SpawnConfig
}

func (f *fakeBootstrapSpawner) Spawn(_ context.Context, cfg ports.SpawnConfig) (domain.Session, int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spawned = append(f.spawned, cfg)
	return domain.Session{ID: "s-1", ProjectID: cfg.ProjectID}, 0, 0, nil
}

type fakeIntakeStore struct {
	projects []domain.ProjectRecord
}

func (f *fakeIntakeStore) ListProjects(_ context.Context) ([]domain.ProjectRecord, error) {
	return f.projects, nil
}
func (f *fakeIntakeStore) ListAllSessions(_ context.Context) ([]domain.SessionRecord, error) {
	return nil, nil
}

func initBareRemoteWithBranch(t *testing.T, branch string, withCommit bool) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "demo-service.git")
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(bare, 0o750); err != nil {
		t.Fatal(err)
	}
	runGit(bare, "init", "--bare", "--initial-branch="+branch)
	if !withCommit {
		return "file://" + bare
	}
	work := t.TempDir()
	runGit(work, "init", "--initial-branch="+branch)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(work, "add", "README.md")
	runGit(work, "commit", "-m", "initial")
	runGit(work, "remote", "add", "origin", bare)
	runGit(work, "push", "-u", "origin", branch)
	return "file://" + bare
}

func TestBootstrapEndToEndInheritanceAndTrackerIntake(t *testing.T) {
	remoteURL := initBareRemoteWithBranch(t, "develop", true)
	root := t.TempDir()
	store := openTestStore(t)
	defaults := domain.ProjectDefaults{
		WorkspaceRoot:         root,
		WorkerAgent:           domain.HarnessClaudeCode,
		OrchestratorAgent:     domain.HarnessCodex,
		Reviewers:             []domain.ReviewerConfig{{Harness: domain.ReviewerAgy}},
		AutoReview:            true,
		AutoReviewSet:         true,
		TrackerIntakeEnabled:  true,
		TrackerIntakeSet:      true,
		TrackerIntakeProvider: domain.TrackerProviderGitHub,
		TrackerIntakeAssignee: "octocat",
	}
	tracker := &fakeBootstrapTracker{
		issues: []domain.Issue{{
			ID:        domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo-service#1"},
			Title:     "Bootstrap smoke issue",
			State:     domain.IssueOpen,
			Assignees: []string{"octocat"},
		}},
	}
	svc := projectsvc.NewWithDeps(projectsvc.Deps{
		Store:             store,
		ProjectDefaults:   defaults,
		TrackerIntakeGate: true,
		WorkerQuota:       &workerquota.Policy{Role: workerquota.RoleWorker, Primary: &workerquota.Target{Harness: domain.HarnessClaudeCode}},
		ReviewerQuota:     &workerquota.Policy{Role: workerquota.RoleReviewer, Primary: &workerquota.Target{Harness: domain.AgentHarness(domain.ReviewerAgy)}},
		Tracker:           tracker,
	})

	res, err := svc.Bootstrap(context.Background(), projectsvc.BootstrapInput{
		RemoteURL: remoteURL,
	})
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if res.Action != projectsvc.BootstrapActionClonedAndRegistered {
		t.Fatalf("Action = %q, want %q", res.Action, projectsvc.BootstrapActionClonedAndRegistered)
	}
	if res.Effective.DefaultBranch != "develop" || res.Effective.DefaultBranchSource != "git_remote" {
		t.Fatalf("DefaultBranch = %q (%q), want develop (git_remote)", res.Effective.DefaultBranch, res.Effective.DefaultBranchSource)
	}
	if !res.Effective.AutoReview || res.Effective.AutoReviewSource != "global" {
		t.Fatalf("AutoReview = %v (%q), want true (global)", res.Effective.AutoReview, res.Effective.AutoReviewSource)
	}
	if !res.Effective.TrackerIntake.Enabled || res.Effective.TrackerIntake.Source != "global" || res.Effective.TrackerIntake.Assignee != "octocat" {
		t.Fatalf("TrackerIntake = %+v", res.Effective.TrackerIntake)
	}
	if !res.Effective.QuotaRouting.WorkerFailover || !res.Effective.QuotaRouting.ReviewerFailover {
		t.Fatalf("QuotaRouting = %+v", res.Effective.QuotaRouting)
	}
	if !res.Readiness.Ready {
		t.Fatalf("Readiness = %+v, want Ready=true", res.Readiness)
	}

	// Verify idempotent retry returns reused_project with no duplicate rows.
	retry, err := svc.Bootstrap(context.Background(), projectsvc.BootstrapInput{RemoteURL: remoteURL})
	if err != nil {
		t.Fatalf("retry Bootstrap() error = %v", err)
	}
	if retry.Action != projectsvc.BootstrapActionReusedProject || retry.Project.ID != res.Project.ID {
		t.Fatalf("retry = %+v, want reused_project %q", retry, res.Project.ID)
	}
	projects, _ := store.ListProjects(context.Background())
	if len(projects) != 1 {
		t.Fatalf("len(projects) = %d, want 1", len(projects))
	}

	// Verify the bootstrapped project accepts a newly assigned issue through trackerintake.
	projects[0].RepoOriginURL = "https://github.com/acme/demo-service.git"
	spawner := &fakeBootstrapSpawner{}
	obs := trackerintake.New(
		trackerintake.SingleTrackerResolver{Adapter: tracker},
		&fakeIntakeStore{projects: projects},
		spawner,
		trackerintake.Config{ProjectDefaults: defaults},
	)
	if err := obs.Poll(context.Background()); err != nil {
		t.Fatalf("obs.Poll() error = %v", err)
	}
	if len(spawner.spawned) != 1 || spawner.spawned[0].ProjectID != res.Project.ID {
		t.Fatalf("spawned = %+v, want 1 spawn for %q", spawner.spawned, res.Project.ID)
	}
}

func TestBootstrapOverridesAndExplicitDisable(t *testing.T) {
	remoteURL := initBareRemoteWithBranch(t, "main", true)
	root := t.TempDir()
	store := openTestStore(t)
	defaults := domain.ProjectDefaults{
		WorkspaceRoot:         root,
		AutoReview:            true,
		AutoReviewSet:         true,
		TrackerIntakeEnabled:  true,
		TrackerIntakeSet:      true,
		TrackerIntakeAssignee: "octocat",
	}
	svc := projectsvc.NewWithDeps(projectsvc.Deps{
		Store:             store,
		ProjectDefaults:   defaults,
		TrackerIntakeGate: true,
	})
	disableAutoReview := false
	res, err := svc.Bootstrap(context.Background(), projectsvc.BootstrapInput{
		RemoteURL: remoteURL,
		Config: &domain.ProjectConfig{
			AutoReviewOverride: &disableAutoReview,
			TrackerIntake:      domain.TrackerIntakeConfig{Disabled: true},
		},
	})
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if res.Effective.AutoReview || res.Effective.AutoReviewSource != "project" {
		t.Fatalf("AutoReview = %v (%q), want false (project)", res.Effective.AutoReview, res.Effective.AutoReviewSource)
	}
	if res.Effective.TrackerIntake.Enabled || res.Effective.TrackerIntake.Source != "project" {
		t.Fatalf("TrackerIntake = %+v, want disabled (project)", res.Effective.TrackerIntake)
	}
}

func TestBootstrapErrorPathsAndSafety(t *testing.T) {
	t.Run("missing workspace root", func(t *testing.T) {
		svc := projectsvc.New(openTestStore(t))
		_, err := svc.Bootstrap(context.Background(), projectsvc.BootstrapInput{RemoteURL: "https://github.com/acme/demo.git"})
		var ae *apierr.Error
		if !errors.As(err, &ae) || ae.Code != "BOOTSTRAP_WORKSPACE_ROOT_UNSET" {
			t.Fatalf("err = %v, want BOOTSTRAP_WORKSPACE_ROOT_UNSET", err)
		}
	})

	t.Run("empty remote repo", func(t *testing.T) {
		remoteURL := initBareRemoteWithBranch(t, "main", false)
		svc := projectsvc.NewWithDeps(projectsvc.Deps{
			Store:           openTestStore(t),
			ProjectDefaults: domain.ProjectDefaults{WorkspaceRoot: t.TempDir()},
		})
		_, err := svc.Bootstrap(context.Background(), projectsvc.BootstrapInput{RemoteURL: remoteURL})
		var ae *apierr.Error
		if !errors.As(err, &ae) || ae.Code != "CLONE_EMPTY_REPOSITORY" {
			t.Fatalf("err = %v, want CLONE_EMPTY_REPOSITORY", err)
		}
	})

	t.Run("dirty existing checkout is never destroyed", func(t *testing.T) {
		remoteURL := initBareRemoteWithBranch(t, "main", true)
		root := t.TempDir()
		target := filepath.Join(root, "demo-service")
		if out, err := exec.Command("git", "clone", remoteURL, target).CombinedOutput(); err != nil {
			t.Fatalf("git clone: %v\n%s", err, out)
		}
		dirtyFile := filepath.Join(target, "uncommitted.txt")
		if err := os.WriteFile(dirtyFile, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		svc := projectsvc.NewWithDeps(projectsvc.Deps{
			Store:           openTestStore(t),
			ProjectDefaults: domain.ProjectDefaults{WorkspaceRoot: root},
		})
		_, err := svc.Bootstrap(context.Background(), projectsvc.BootstrapInput{RemoteURL: remoteURL})
		var ae *apierr.Error
		if !errors.As(err, &ae) || ae.Code != "CHECKOUT_DIRTY" {
			t.Fatalf("err = %v, want CHECKOUT_DIRTY", err)
		}
		if _, statErr := os.Stat(dirtyFile); statErr != nil {
			t.Fatalf("dirty file was removed or altered: %v", statErr)
		}
	})

	t.Run("reuses clean existing checkout after partial failure", func(t *testing.T) {
		remoteURL := initBareRemoteWithBranch(t, "main", true)
		root := t.TempDir()
		target := filepath.Join(root, "demo-service")
		if out, err := exec.Command("git", "clone", remoteURL, target).CombinedOutput(); err != nil {
			t.Fatalf("git clone: %v\n%s", err, out)
		}
		svc := projectsvc.NewWithDeps(projectsvc.Deps{
			Store:           openTestStore(t),
			ProjectDefaults: domain.ProjectDefaults{WorkspaceRoot: root},
		})
		res, err := svc.Bootstrap(context.Background(), projectsvc.BootstrapInput{RemoteURL: remoteURL})
		if err != nil {
			t.Fatalf("Bootstrap() error = %v", err)
		}
		if res.Action != projectsvc.BootstrapActionReusedCheckoutAndRegistered {
			t.Fatalf("Action = %q, want %q", res.Action, projectsvc.BootstrapActionReusedCheckoutAndRegistered)
		}
	})

	t.Run("concurrent bootstrap requests serialize cleanly", func(t *testing.T) {
		remoteURL := initBareRemoteWithBranch(t, "main", true)
		root := t.TempDir()
		store := openTestStore(t)
		svc := projectsvc.NewWithDeps(projectsvc.Deps{
			Store:           store,
			ProjectDefaults: domain.ProjectDefaults{WorkspaceRoot: root},
		})
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				_, errs[idx] = svc.Bootstrap(context.Background(), projectsvc.BootstrapInput{RemoteURL: remoteURL})
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("goroutine %d failed: %v", i, err)
			}
		}
		projects, _ := store.ListProjects(context.Background())
		if len(projects) != 1 {
			t.Fatalf("len(projects) = %d, want 1", len(projects))
		}
	})

	t.Run("readiness reports daemon gate blocker without secrets", func(t *testing.T) {
		remoteURL := initBareRemoteWithBranch(t, "main", true)
		svc := projectsvc.NewWithDeps(projectsvc.Deps{
			Store: openTestStore(t),
			ProjectDefaults: domain.ProjectDefaults{
				WorkspaceRoot:         t.TempDir(),
				TrackerIntakeEnabled:  true,
				TrackerIntakeSet:      true,
				TrackerIntakeAssignee: "octocat",
			},
			TrackerIntakeGate: false,
		})
		res, err := svc.Bootstrap(context.Background(), projectsvc.BootstrapInput{RemoteURL: remoteURL})
		if err != nil {
			t.Fatalf("Bootstrap() error = %v", err)
		}
		if res.Readiness.Ready || res.Readiness.TrackerOK {
			t.Fatalf("Readiness = %+v, want Ready=false TrackerOK=false", res.Readiness)
		}
		joined := strings.Join(res.Readiness.Blockers, "; ")
		if !strings.Contains(joined, "AO_TRACKER_INTAKE") {
			t.Fatalf("Blockers = %q, want AO_TRACKER_INTAKE mention", joined)
		}
	})

	t.Run("tracker preflight failure marks not ready with sanitized blocker", func(t *testing.T) {
		remoteURL := initBareRemoteWithBranch(t, "main", true)
		svc := projectsvc.NewWithDeps(projectsvc.Deps{
			Store: openTestStore(t),
			ProjectDefaults: domain.ProjectDefaults{
				WorkspaceRoot:         t.TempDir(),
				TrackerIntakeEnabled:  true,
				TrackerIntakeSet:      true,
				TrackerIntakeAssignee: "octocat",
			},
			TrackerIntakeGate: true,
			Tracker:           &fakeBootstrapTracker{preflightErr: errors.New("401 unauthorized ghp_SUPERSECRETTOKEN123")},
		})
		res, err := svc.Bootstrap(context.Background(), projectsvc.BootstrapInput{RemoteURL: remoteURL})
		if err != nil {
			t.Fatalf("Bootstrap() error = %v", err)
		}
		if res.Readiness.Ready || res.Readiness.TrackerOK {
			t.Fatalf("Readiness = %+v, want Ready=false TrackerOK=false", res.Readiness)
		}
		joined := strings.Join(res.Readiness.Blockers, "; ")
		if !strings.Contains(joined, "tracker credential preflight failed") {
			t.Fatalf("Blockers = %q, want sanitized preflight failure", joined)
		}
		if strings.Contains(joined, "ghp_SUPERSECRETTOKEN123") {
			t.Fatalf("Blockers leaked secret token: %q", joined)
		}
	})

	t.Run("embedded credentials in remote URL are rejected", func(t *testing.T) {
		svc := projectsvc.NewWithDeps(projectsvc.Deps{
			Store:           openTestStore(t),
			ProjectDefaults: domain.ProjectDefaults{WorkspaceRoot: t.TempDir()},
		})
		_, err := svc.Bootstrap(context.Background(), projectsvc.BootstrapInput{
			RemoteURL: "https://x-access-token:ghp_SECRET123@github.com/acme/demo.git",
		})
		var ae *apierr.Error
		if !errors.As(err, &ae) || ae.Code != "CLONE_URL_EMBEDDED_CREDENTIALS" {
			t.Fatalf("err = %v, want CLONE_URL_EMBEDDED_CREDENTIALS", err)
		}
	})

	t.Run("explicit global tracker intake off reports global provenance", func(t *testing.T) {
		remoteURL := initBareRemoteWithBranch(t, "main", true)
		svc := projectsvc.NewWithDeps(projectsvc.Deps{
			Store: openTestStore(t),
			ProjectDefaults: domain.ProjectDefaults{
				WorkspaceRoot:        t.TempDir(),
				TrackerIntakeEnabled: false,
				TrackerIntakeSet:     true,
			},
		})
		res, err := svc.Bootstrap(context.Background(), projectsvc.BootstrapInput{RemoteURL: remoteURL})
		if err != nil {
			t.Fatalf("Bootstrap() error = %v", err)
		}
		if res.Effective.TrackerIntake.Enabled || res.Effective.TrackerIntake.Source != "global" {
			t.Fatalf("TrackerIntake = %+v, want enabled=false source=global", res.Effective.TrackerIntake)
		}
	})
}
