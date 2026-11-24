package sessionmanager

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

func automaticQuotaWorker(cfg ports.SpawnConfig) bool {
	return cfg.Kind == domain.KindWorker && cfg.ProjectID != "" &&
		(cfg.CreatedBy == domain.SessionCreatedByAutomation || cfg.AutomationRunID != nil || cfg.ParentSessionID != "") &&
		cfg.Harness == "" && cfg.AgentAccountID == "" && cfg.AgentConfig.Model == "" &&
		cfg.AgentConfig.Mode == "" && cfg.AgentConfig.Effort == "" && !cfg.EffortOverride && !cfg.AgentConfigResolved
}

func (m *Manager) selectWorkerQuotaTarget(ctx context.Context, cfg *ports.SpawnConfig, project *domain.ProjectRecord) ([]workerquota.Target, error) {
	policy := m.workerQuota
	if policy == nil || !automaticQuotaWorker(*cfg) {
		return nil, nil
	}
	harness := effectiveHarness("", cfg.Kind, project.Config)
	base := effectiveAgentConfig(harness, cfg.Kind, project.Config)
	primary := workerquota.Target{Harness: harness, Model: base.Model, Effort: base.Effort}
	targets := policy.Targets(primary)
	if len(targets) == 0 {
		return nil, nil
	}
	defaults := policy.DefaultModels(ctx, *project, targets)
	windows := m.workerQuotaWindows(ctx, targets, defaults)
	var selected workerquota.Target
	var remaining []workerquota.Target
	var blocked bool
	for i, route := range targets {
		if _, ok := workerquota.Select([]workerquota.Target{route}, windows, m.clock()); !ok {
			blocked = true
			continue
		}
		if route == primary || m.quotaTargetReady(ctx, route, project.Config, *cfg) {
			selected, remaining = route, targets[i+1:]
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if selected.Harness == "" {
		if blocked {
			return nil, workerquota.ErrDeferred
		}
		return nil, nil
	}
	if selected != primary {
		cfg.Harness = selected.Harness
		cfg.AgentConfig.Model, cfg.AgentConfig.Effort = selected.Model, selected.Effort
		cfg.EffortOverride = selected.Effort != ""
		// Remove provider-specific inherited tuning from this copy of the
		// project while preserving worker permissions and other project rules.
		project.Config.AgentConfig.Model, project.Config.AgentConfig.Effort, project.Config.AgentConfig.Mode = "", "", ""
		project.Config.Worker.AgentConfig.Model, project.Config.Worker.AgentConfig.Effort, project.Config.Worker.AgentConfig.Mode = "", "", ""
		m.logger.Info("worker quota selected configured route", "project", cfg.ProjectID, "harness", selected.Harness, "model", selected.Model, "effort", selected.Effort)
	}
	if selected.Model == "" && defaults[selected.Harness] != "" {
		cfg.AgentConfig.Model = defaults[selected.Harness]
		cfg.AgentConfig.Effort = selected.Effort
	}
	return remaining, nil
}

func (m *Manager) workerQuotaWindows(ctx context.Context, targets []workerquota.Target, defaults map[domain.AgentHarness]string) map[workerquota.Target][]workerquota.Window {
	now := m.clock()
	var failures []workerquota.Failure
	if store, ok := m.store.(interface {
		WorkerQuotaFailures(context.Context, time.Time) ([]workerquota.Failure, error)
	}); ok {
		var err error
		failures, err = store.WorkerQuotaFailures(ctx, now)
		if err != nil {
			m.logger.Warn("worker quota failure records unavailable", "error", err)
		}
	}
	windows, err := m.workerQuota.Windows(ctx, targets, failures, now, defaults)
	if err != nil {
		m.logger.Debug("worker quota source unavailable", "error", err)
	}
	return windows
}

func (m *Manager) quotaTargetReady(ctx context.Context, target workerquota.Target, project domain.ProjectConfig, cfg ports.SpawnConfig) bool {
	if _, ok := m.agents.Agent(target.Harness); !ok || validateSpawnModel(target.Harness, target.Model) != nil {
		return false
	}
	if m.agentReadiness != nil {
		readiness, err := m.agentReadiness.EnsureAgentReadiness(ctx, string(target.Harness), domain.AgentReadinessPurposeLaunch)
		if err != nil || readiness.Installation.State == domain.AgentInstallationNotInstalled ||
			(readiness.Authentication.State == domain.AgentAuthenticationUnauthorized && readiness.Authentication.Freshness == domain.AgentReadinessFresh) {
			return false
		}
	}
	if cfg.RequestedMode == domain.SessionModeChat {
		permissions := applySpawnAgentConfig(effectiveAgentConfig(target.Harness, cfg.Kind, project), cfg.AgentConfig).Permissions
		return m.chat != nil && m.chat.PreflightChat(ctx, target.Harness, permissions) == nil
	}
	return true
}
