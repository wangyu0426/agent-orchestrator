package config

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestLoadProjectDefaults(t *testing.T) {
	t.Setenv("AO_PROJECTS_ROOT", "/tmp/ao-projects")
	t.Setenv("AO_DEFAULT_WORKER_AGENT", "claude-code")
	t.Setenv("AO_DEFAULT_ORCHESTRATOR_AGENT", "codex")
	t.Setenv("AO_DEFAULT_REVIEWERS", "claude-code,agy")
	t.Setenv("AO_DEFAULT_AUTO_REVIEW", "on")
	t.Setenv("AO_DEFAULT_TRACKER_INTAKE_ENABLED", "on")
	t.Setenv("AO_DEFAULT_TRACKER_INTAKE_PROVIDER", "github")
	t.Setenv("AO_DEFAULT_TRACKER_INTAKE_ASSIGNEE", "octocat")

	d, err := loadProjectDefaults()
	if err != nil {
		t.Fatalf("loadProjectDefaults() error = %v", err)
	}
	if d.WorkspaceRoot != "/tmp/ao-projects" {
		t.Fatalf("WorkspaceRoot = %q, want /tmp/ao-projects", d.WorkspaceRoot)
	}
	if d.WorkerAgent != domain.HarnessClaudeCode {
		t.Fatalf("WorkerAgent = %q, want claude-code", d.WorkerAgent)
	}
	if d.OrchestratorAgent != domain.HarnessCodex {
		t.Fatalf("OrchestratorAgent = %q, want codex", d.OrchestratorAgent)
	}
	if len(d.Reviewers) != 2 || d.Reviewers[0].Harness != domain.ReviewerClaudeCode || d.Reviewers[1].Harness != domain.ReviewerAgy {
		t.Fatalf("Reviewers = %+v, want [claude-code agy]", d.Reviewers)
	}
	if !d.AutoReviewSet || !d.AutoReview {
		t.Fatalf("AutoReview = (%v, set=%v), want (true, true)", d.AutoReview, d.AutoReviewSet)
	}
	if !d.TrackerIntakeSet || !d.TrackerIntakeEnabled {
		t.Fatalf("TrackerIntake = (%v, set=%v), want (true, true)", d.TrackerIntakeEnabled, d.TrackerIntakeSet)
	}
	if d.TrackerIntakeProvider != domain.TrackerProviderGitHub || d.TrackerIntakeAssignee != "octocat" {
		t.Fatalf("TrackerIntake provider/assignee = (%q, %q)", d.TrackerIntakeProvider, d.TrackerIntakeAssignee)
	}
}

func TestLoadProjectDefaultsValidation(t *testing.T) {
	cases := []struct {
		name string
		env  string
		val  string
	}{
		{"relative root", "AO_PROJECTS_ROOT", "relative/path"},
		{"unknown worker", "AO_DEFAULT_WORKER_AGENT", "nope"},
		{"unknown orchestrator", "AO_DEFAULT_ORCHESTRATOR_AGENT", "nope"},
		{"unknown reviewer", "AO_DEFAULT_REVIEWERS", "nope"},
		{"bad auto review toggle", "AO_DEFAULT_AUTO_REVIEW", "maybe"},
		{"bad tracker provider", "AO_DEFAULT_TRACKER_INTAKE_PROVIDER", "jira"},
		{"whitespace assignee", "AO_DEFAULT_TRACKER_INTAKE_ASSIGNEE", "alice bob"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.val)
			if _, err := loadProjectDefaults(); err == nil {
				t.Fatalf("loadProjectDefaults() with %s=%q succeeded, want error", tc.env, tc.val)
			}
		})
	}
}
