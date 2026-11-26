package domain

import "strings"

// ProjectDefaults holds daemon-wide default settings inherited by projects when
// a project leaves the corresponding field unset. Secrets and account bindings
// never live here and are never copied into per-project config.
type ProjectDefaults struct {
	// WorkspaceRoot is the approved parent directory used when bootstrapping a
	// remote repository without an explicit destination parent.
	WorkspaceRoot string
	// WorkerAgent is the default worker harness when neither project config nor
	// global worker quota primary specifies one.
	WorkerAgent AgentHarness
	// OrchestratorAgent is the default orchestrator harness when neither project
	// config nor global orchestrator quota primary specifies one.
	OrchestratorAgent AgentHarness
	// Reviewers is the default reviewer list when neither project config nor
	// global reviewer quota primary specifies one.
	Reviewers []ReviewerConfig
	// AutoReview is the daemon-wide default for automatic PR review on new
	// worker sessions. AutoReviewSet distinguishes an explicit daemon setting
	// from unset.
	AutoReview    bool
	AutoReviewSet bool
	// TrackerIntakeEnabled is the daemon-wide default for per-project tracker
	// intake when a project does not explicitly enable or disable it.
	// Still gated by the daemon-wide AO_TRACKER_INTAKE kill switch.
	TrackerIntakeEnabled  bool
	TrackerIntakeSet      bool
	TrackerIntakeProvider TrackerProvider
	TrackerIntakeAssignee string
}

// EffectiveAutoReview resolves the effective auto-review setting for a project
// and reports its precedence source ("project", "global", or "default").
func (c ProjectConfig) EffectiveAutoReview(defaults ProjectDefaults) (bool, string) {
	if c.AutoReview {
		return true, "project"
	}
	if c.AutoReviewOverride != nil {
		return *c.AutoReviewOverride, "project"
	}
	if defaults.AutoReviewSet {
		return defaults.AutoReview, "global"
	}
	return false, "default"
}

// EffectiveTrackerIntake resolves the effective tracker-intake configuration for
// a project and reports its precedence source ("project", "global", or "default").
// Explicit project disable (Disabled: true) always wins over global defaults.
func (c ProjectConfig) EffectiveTrackerIntake(defaults ProjectDefaults) (TrackerIntakeConfig, string) {
	if c.TrackerIntake.Disabled {
		return TrackerIntakeConfig{Disabled: true}, "project"
	}
	if c.TrackerIntake.Enabled {
		out := c.TrackerIntake
		if out.Provider == "" {
			out.Provider = defaults.TrackerIntakeProvider
		}
		if strings.TrimSpace(out.Assignee) == "" {
			out.Assignee = defaults.TrackerIntakeAssignee
		}
		return out, "project"
	}
	if defaults.TrackerIntakeSet {
		if !defaults.TrackerIntakeEnabled {
			return TrackerIntakeConfig{}, "global"
		}
		out := TrackerIntakeConfig{
			Enabled:  true,
			Provider: c.TrackerIntake.Provider,
			Repo:     c.TrackerIntake.Repo,
			Assignee: c.TrackerIntake.Assignee,
		}
		if out.Provider == "" {
			out.Provider = defaults.TrackerIntakeProvider
		}
		if strings.TrimSpace(out.Assignee) == "" {
			out.Assignee = defaults.TrackerIntakeAssignee
		}
		return out, "global"
	}
	return c.TrackerIntake, "default"
}

// EffectiveWorkerAgent resolves the effective worker harness and its source
// ("project", "quota_primary", "global", or "default").
func (c ProjectConfig) EffectiveWorkerAgent(defaults ProjectDefaults, quotaPrimary AgentHarness) (AgentHarness, string) {
	if c.Worker.Harness != "" {
		return c.Worker.Harness, "project"
	}
	if quotaPrimary != "" {
		return quotaPrimary, "quota_primary"
	}
	if defaults.WorkerAgent != "" {
		return defaults.WorkerAgent, "global"
	}
	return "", "default"
}

// EffectiveOrchestratorAgent resolves the effective orchestrator harness and its
// source ("project", "quota_primary", "global", or "default").
func (c ProjectConfig) EffectiveOrchestratorAgent(defaults ProjectDefaults, quotaPrimary AgentHarness) (AgentHarness, string) {
	if c.Orchestrator.Harness != "" {
		return c.Orchestrator.Harness, "project"
	}
	if quotaPrimary != "" {
		return quotaPrimary, "quota_primary"
	}
	if defaults.OrchestratorAgent != "" {
		return defaults.OrchestratorAgent, "global"
	}
	return "", "default"
}

// EffectiveReviewers resolves the effective reviewer list and its source
// ("project", "quota_primary", "global", "worker_inherited", or "default").
func (c ProjectConfig) EffectiveReviewers(defaults ProjectDefaults, quotaPrimary ReviewerHarness) ([]ReviewerConfig, string) {
	if len(c.Reviewers) > 0 {
		out := make([]ReviewerConfig, len(c.Reviewers))
		copy(out, c.Reviewers)
		return out, "project"
	}
	if quotaPrimary != "" {
		return []ReviewerConfig{{Harness: quotaPrimary}}, "quota_primary"
	}
	if len(defaults.Reviewers) > 0 {
		out := make([]ReviewerConfig, len(defaults.Reviewers))
		copy(out, defaults.Reviewers)
		return out, "global"
	}
	if h, cfg := c.DefaultWorkerReviewer(); h != "" {
		return []ReviewerConfig{{Harness: h, AgentConfig: cfg}}, "worker_inherited"
	}
	return []ReviewerConfig{{Harness: FallbackReviewerHarness}}, "default"
}
