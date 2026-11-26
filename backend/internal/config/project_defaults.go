package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func loadProjectDefaults() (domain.ProjectDefaults, error) {
	var d domain.ProjectDefaults

	if raw, ok := os.LookupEnv("AO_PROJECTS_ROOT"); ok && strings.TrimSpace(raw) != "" {
		root := filepath.Clean(strings.TrimSpace(raw))
		if !filepath.IsAbs(root) {
			return domain.ProjectDefaults{}, fmt.Errorf("invalid AO_PROJECTS_ROOT %q: must be an absolute path", raw)
		}
		d.WorkspaceRoot = root
	}

	if raw, ok := os.LookupEnv("AO_DEFAULT_WORKER_AGENT"); ok && strings.TrimSpace(raw) != "" {
		h := domain.AgentHarness(strings.TrimSpace(raw))
		if !h.IsKnown() {
			return domain.ProjectDefaults{}, fmt.Errorf("invalid AO_DEFAULT_WORKER_AGENT %q: unknown harness", raw)
		}
		d.WorkerAgent = h
	}

	if raw, ok := os.LookupEnv("AO_DEFAULT_ORCHESTRATOR_AGENT"); ok && strings.TrimSpace(raw) != "" {
		h := domain.AgentHarness(strings.TrimSpace(raw))
		if !h.IsKnown() {
			return domain.ProjectDefaults{}, fmt.Errorf("invalid AO_DEFAULT_ORCHESTRATOR_AGENT %q: unknown harness", raw)
		}
		d.OrchestratorAgent = h
	}

	if raw, ok := os.LookupEnv("AO_DEFAULT_REVIEWERS"); ok && strings.TrimSpace(raw) != "" {
		parts := strings.Split(raw, ",")
		reviewers := make([]domain.ReviewerConfig, 0, len(parts))
		for _, part := range parts {
			name := strings.TrimSpace(part)
			if name == "" {
				continue
			}
			h := domain.ReviewerHarness(name)
			if !h.IsKnown() {
				return domain.ProjectDefaults{}, fmt.Errorf("invalid AO_DEFAULT_REVIEWERS %q: unknown reviewer harness %q", raw, name)
			}
			reviewers = append(reviewers, domain.ReviewerConfig{Harness: h})
		}
		d.Reviewers = reviewers
	}

	if raw, ok := os.LookupEnv("AO_DEFAULT_AUTO_REVIEW"); ok && strings.TrimSpace(raw) != "" {
		v, err := parseToggleEnv("AO_DEFAULT_AUTO_REVIEW", raw)
		if err != nil {
			return domain.ProjectDefaults{}, err
		}
		d.AutoReview = v
		d.AutoReviewSet = true
	}

	if raw, ok := os.LookupEnv("AO_DEFAULT_TRACKER_INTAKE_ENABLED"); ok && strings.TrimSpace(raw) != "" {
		v, err := parseToggleEnv("AO_DEFAULT_TRACKER_INTAKE_ENABLED", raw)
		if err != nil {
			return domain.ProjectDefaults{}, err
		}
		d.TrackerIntakeEnabled = v
		d.TrackerIntakeSet = true
	}

	if raw, ok := os.LookupEnv("AO_DEFAULT_TRACKER_INTAKE_PROVIDER"); ok && strings.TrimSpace(raw) != "" {
		p := domain.TrackerProvider(strings.TrimSpace(raw))
		if p != domain.TrackerProviderGitHub && p != domain.TrackerProviderGitLab {
			return domain.ProjectDefaults{}, fmt.Errorf("invalid AO_DEFAULT_TRACKER_INTAKE_PROVIDER %q: must be github or gitlab", raw)
		}
		d.TrackerIntakeProvider = p
	}

	if raw, ok := os.LookupEnv("AO_DEFAULT_TRACKER_INTAKE_ASSIGNEE"); ok && strings.TrimSpace(raw) != "" {
		trimmed := strings.TrimSpace(raw)
		if strings.ContainsAny(trimmed, " \t\r\n") {
			return domain.ProjectDefaults{}, fmt.Errorf("invalid AO_DEFAULT_TRACKER_INTAKE_ASSIGNEE %q: must not contain whitespace", raw)
		}
		d.TrackerIntakeAssignee = trimmed
	}

	return d, nil
}
