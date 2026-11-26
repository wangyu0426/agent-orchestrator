package project

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

// BootstrapAction values report what Service.Bootstrap did for the requested repository.
const (
	BootstrapActionClonedAndRegistered         = "cloned_and_registered"
	BootstrapActionReusedCheckoutAndRegistered = "reused_checkout_and_registered"
	BootstrapActionReusedProject               = "reused_project"
)

// Bootstrap takes an authorized Git repository from URL to a registered,
// ready-to-dispatch project. It clones into DestinationParent (or the daemon's
// configured AO_PROJECTS_ROOT), safely reuses a clean matching checkout on
// retry, never duplicates an already-registered project, and returns a truthful
// effective-policy and readiness preflight without leaking secrets.
func (m *Service) Bootstrap(ctx context.Context, in BootstrapInput) (BootstrapResult, error) {
	remoteURL := normalizeCloneURL(in.RemoteURL)
	repoName, err := cloneRepositoryName(remoteURL)
	if err != nil {
		return BootstrapResult{}, err
	}

	parentRaw := strings.TrimSpace(in.DestinationParent)
	if parentRaw == "" {
		parentRaw = strings.TrimSpace(m.projectDefaults.WorkspaceRoot)
	}
	if parentRaw == "" {
		return BootstrapResult{}, apierr.Invalid(
			"BOOTSTRAP_WORKSPACE_ROOT_UNSET",
			"No workspace root is configured. Set AO_PROJECTS_ROOT on the daemon or pass --destination.",
			nil,
		)
	}
	parent, err := normalizePath(parentRaw)
	if err != nil {
		return BootstrapResult{}, err
	}
	if in.Config != nil {
		if err := in.Config.Validate(); err != nil {
			return BootstrapResult{}, apierr.Invalid("INVALID_PROJECT_CONFIG", err.Error(), nil)
		}
	}
	if in.ProjectID != nil {
		if err := validateProjectID(domain.ProjectID(strings.TrimSpace(*in.ProjectID))); err != nil {
			return BootstrapResult{}, err
		}
	}

	target := filepath.Join(parent, repoName)
	if err := validateRepositorySetupPathSafety(target); err != nil {
		return BootstrapResult{}, err
	}

	unlock := m.lockBootstrapTarget(target)
	defer unlock()

	// 1. Idempotent retry: if already registered at target, verify origin and return.
	if existing, ok, err := m.findRegisteredProjectAtPath(ctx, target); err != nil {
		return BootstrapResult{}, apierr.Internal("PROJECT_LOAD_FAILED", "Failed to load project")
	} else if ok {
		if !sameNormalizedOrigin(existing.RepoOriginURL, remoteURL) {
			return BootstrapResult{}, apierr.Conflict(
				"CLONE_DESTINATION_EXISTS",
				"A different project is already registered at this destination.",
				map[string]any{"path": target, "existingProjectId": existing.ID},
			)
		}
		proj := m.projectFromRow(ctx, existing)
		return m.buildBootstrapResult(ctx, BootstrapActionReusedProject, proj), nil
	}

	// 2. Inspect target on disk: reuse a clean matching checkout or clone fresh.
	info, statErr := os.Lstat(target)
	if statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return BootstrapResult{}, apierr.Conflict(
				"CLONE_DESTINATION_EXISTS",
				"A non-directory or symlink already exists at this destination.",
				map[string]any{"path": target},
			)
		}
		if !isGitRepo(target) {
			return BootstrapResult{}, apierr.Conflict(
				"CLONE_DESTINATION_EXISTS",
				"A folder with this repository name already exists and is not a Git repository.",
				map[string]any{"path": target},
			)
		}
		origin := resolveGitOriginURL(target)
		if !sameNormalizedOrigin(origin, remoteURL) {
			return BootstrapResult{}, apierr.Conflict(
				"CLONE_DESTINATION_EXISTS",
				"An existing Git checkout at this destination points to a different remote.",
				map[string]any{"path": target},
			)
		}
		if !repoHasCommit(ctx, target) {
			return BootstrapResult{}, apierr.Invalid(
				"CLONE_EMPTY_REPOSITORY",
				"Orchestrator needs a repository with at least one commit.",
				map[string]any{"path": target},
			)
		}
		clean, err := isWorkingTreeClean(ctx, target)
		if err != nil {
			return BootstrapResult{}, apierr.Invalid(
				"CHECKOUT_STATUS_FAILED",
				"Orchestrator could not inspect the existing checkout status.",
				map[string]any{"path": target},
			)
		}
		if !clean {
			return BootstrapResult{}, apierr.Conflict(
				"CHECKOUT_DIRTY",
				"Existing checkout has uncommitted changes. Clean or commit them, or choose another destination.",
				map[string]any{"path": target},
			)
		}
		proj, err := m.Add(ctx, AddInput{
			Path:      target,
			ProjectID: in.ProjectID,
			Name:      in.Name,
			Config:    in.Config,
		})
		if err != nil {
			return BootstrapResult{}, err
		}
		return m.buildBootstrapResult(ctx, BootstrapActionReusedCheckoutAndRegistered, proj), nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return BootstrapResult{}, apierr.Invalid(
			"CLONE_DESTINATION_UNAVAILABLE",
			"The clone destination could not be inspected.",
			map[string]any{"path": target},
		)
	}

	// 3. Target does not exist: clone into staging dir, validate commits, register.
	prepared, err := m.prepareClone(ctx, CloneInput{
		RemoteURL:         remoteURL,
		DestinationParent: parent,
		ProjectID:         in.ProjectID,
		Name:              in.Name,
		Config:            in.Config,
	}, nil)
	if err != nil {
		return BootstrapResult{}, err
	}
	if !repoHasCommit(ctx, prepared.Path) {
		m.cleanupPreparedCloneAfterFailure(ctx, prepared)
		return BootstrapResult{}, apierr.Invalid(
			"CLONE_EMPTY_REPOSITORY",
			"Orchestrator needs a repository with at least one commit.",
			nil,
		)
	}
	proj, err := m.Add(ctx, AddInput{
		Path:               prepared.Path,
		ProjectID:          in.ProjectID,
		Name:               in.Name,
		Config:             in.Config,
		ClonePreparationID: prepared.PreparationID,
	})
	if err != nil {
		m.cleanupPreparedCloneAfterFailure(ctx, prepared)
		return BootstrapResult{}, err
	}
	return m.buildBootstrapResult(ctx, BootstrapActionClonedAndRegistered, proj), nil
}

func (m *Service) lockBootstrapTarget(target string) func() {
	m.bootstrapLocksMu.Lock()
	if m.bootstrapLocks == nil {
		m.bootstrapLocks = make(map[string]*sync.Mutex)
	}
	mu, ok := m.bootstrapLocks[target]
	if !ok {
		mu = &sync.Mutex{}
		m.bootstrapLocks[target] = mu
	}
	m.bootstrapLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (m *Service) findRegisteredProjectAtPath(ctx context.Context, path string) (domain.ProjectRecord, bool, error) {
	if existing, ok, err := m.store.FindProjectByPath(ctx, path); err != nil {
		return domain.ProjectRecord{}, false, err
	} else if ok {
		return existing, true, nil
	}
	selectedInfo, statErr := os.Stat(path)
	if statErr != nil || !selectedInfo.IsDir() {
		return domain.ProjectRecord{}, false, nil //nolint:nilerr // Missing or non-directory path simply means no existing alias matches.
	}
	activeProjects, err := m.store.ListProjects(ctx)
	if err != nil {
		return domain.ProjectRecord{}, false, err
	}
	for _, candidate := range activeProjects {
		if registeredInfo, err := os.Stat(candidate.Path); err == nil && os.SameFile(selectedInfo, registeredInfo) {
			return candidate, true, nil
		}
	}
	return domain.ProjectRecord{}, false, nil
}

func sameNormalizedOrigin(a, b string) bool {
	na := normalizeCloneURL(strings.TrimSpace(a))
	nb := normalizeCloneURL(strings.TrimSpace(b))
	if na == "" || nb == "" {
		return false
	}
	return na == nb
}

func isWorkingTreeClean(ctx context.Context, repoPath string) (bool, error) {
	cmd := aoprocess.CommandContext(ctx, "git", "-C", repoPath, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "", nil
}

func sanitizeRepoURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

func (m *Service) buildBootstrapResult(ctx context.Context, action string, proj Project) BootstrapResult {
	proj.Repo = sanitizeRepoURL(proj.Repo)

	var cfg domain.ProjectConfig
	if proj.Config != nil {
		cfg = *proj.Config
	}

	defaultBranchSource := "git_remote"
	if cfg.DefaultBranch != "" && cfg.DefaultBranch != domain.DefaultBranchAuto {
		defaultBranchSource = "project"
	} else if proj.DefaultBranch == "" {
		defaultBranchSource = "default"
	}

	var workerPrimary, orchPrimary domain.AgentHarness
	var reviewerPrimary domain.ReviewerHarness
	if m.workerQuota != nil && m.workerQuota.Primary != nil {
		workerPrimary = m.workerQuota.Primary.Harness
	}
	if m.orchestratorQuota != nil && m.orchestratorQuota.Primary != nil {
		orchPrimary = m.orchestratorQuota.Primary.Harness
	}
	if m.reviewerQuota != nil && m.reviewerQuota.Primary != nil {
		reviewerPrimary = domain.ReviewerHarness(m.reviewerQuota.Primary.Harness)
	}

	workerAgent, workerSource := cfg.EffectiveWorkerAgent(m.projectDefaults, workerPrimary)
	if workerAgent == "" {
		workerAgent = m.defaultHarness
		workerSource = "default"
	}
	orchAgent, orchSource := cfg.EffectiveOrchestratorAgent(m.projectDefaults, orchPrimary)
	reviewers, reviewersSource := cfg.EffectiveReviewers(m.projectDefaults, reviewerPrimary)
	autoReview, autoReviewSource := cfg.EffectiveAutoReview(m.projectDefaults)
	effectiveIntake, intakeSource := cfg.EffectiveTrackerIntake(m.projectDefaults)

	effectiveProvider := effectiveIntake.Provider
	if effectiveProvider == "" && effectiveIntake.Enabled {
		effectiveProvider = domain.InferTrackerProvider(proj.Repo)
	}
	effectiveRepo := effectiveIntake.Repo
	if effectiveRepo == "" {
		if id, err := domain.ParseRepositoryIdentity(proj.Repo); err == nil {
			effectiveRepo = id.Namespace + "/" + id.Name
		}
	}

	quotumConfigured := hasQuotumSource(m.workerQuota) || hasQuotumSource(m.reviewerQuota) || hasQuotumSource(m.orchestratorQuota)

	effective := EffectiveProjectPolicy{
		DefaultBranch:           proj.DefaultBranch,
		DefaultBranchSource:     defaultBranchSource,
		WorkerAgent:             workerAgent,
		WorkerAgentSource:       workerSource,
		OrchestratorAgent:       orchAgent,
		OrchestratorAgentSource: orchSource,
		Reviewers:               reviewers,
		ReviewersSource:         reviewersSource,
		AutoReview:              autoReview,
		AutoReviewSource:        autoReviewSource,
		TrackerIntake: EffectiveTrackerIntakePolicy{
			Enabled:           effectiveIntake.Enabled,
			DaemonGateEnabled: m.trackerIntakeGate,
			Provider:          effectiveProvider,
			Repo:              effectiveRepo,
			Assignee:          effectiveIntake.Assignee,
			Source:            intakeSource,
		},
		QuotaRouting: EffectiveQuotaPolicy{
			WorkerFailover:       m.workerQuota != nil,
			ReviewerFailover:     m.reviewerQuota != nil,
			OrchestratorFailover: m.orchestratorQuota != nil,
			QuotumConfigured:     quotumConfigured,
		},
	}

	readiness := BootstrapReadiness{
		GitOK:     true,
		SCMOK:     true,
		TrackerOK: true,
		AgentOK:   workerAgent.IsKnown(),
	}
	if !readiness.AgentOK {
		readiness.Blockers = append(readiness.Blockers, fmt.Sprintf("resolved worker agent %q is not a known harness", workerAgent))
	}

	// SCM metadata check for github.com origins when a resolver is wired.
	if m.githubRepos != nil {
		if id, err := domain.ParseRepositoryIdentity(proj.Repo); err == nil && id.Host == "github.com" {
			if _, err := m.githubRepos.ResolveGitHubRepository(ctx, id.Namespace, id.Name); err != nil {
				readiness.SCMOK = false
				readiness.Blockers = append(readiness.Blockers, "GitHub repository metadata check failed; verify GitHub credentials and repository access.")
			}
		}
	}

	// Tracker intake readiness checks.
	if effectiveIntake.Enabled {
		if !m.trackerIntakeGate {
			readiness.TrackerOK = false
			readiness.Blockers = append(readiness.Blockers, "tracker intake is enabled for this project, but AO_TRACKER_INTAKE is off on the daemon.")
		}
		if err := effectiveIntake.Validate(); err != nil {
			readiness.TrackerOK = false
			readiness.Blockers = append(readiness.Blockers, fmt.Sprintf("tracker intake configuration is incomplete: %s", err.Error()))
		}
		if m.tracker == nil {
			readiness.TrackerOK = false
			readiness.Blockers = append(readiness.Blockers, "no tracker adapter is configured on the daemon; verify GitHub/GitLab credentials.")
		}
	}

	readiness.Ready = readiness.GitOK && readiness.SCMOK && readiness.TrackerOK && readiness.AgentOK && len(readiness.Blockers) == 0

	return BootstrapResult{
		Action:    action,
		Project:   proj,
		Effective: effective,
		Readiness: readiness,
	}
}

func hasQuotumSource(p *workerquota.Policy) bool {
	return p != nil && p.Source != nil
}
