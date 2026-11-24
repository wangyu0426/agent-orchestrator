package sessionmanager

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

// AutoSpawnAdmitter checks and reserves automatic-launch breaker state for
// the resolved launch configuration.
type AutoSpawnAdmitter interface {
	TryAdmit(harness domain.AgentHarness, model, effort string, mode domain.SessionMode) (admitted bool, pauseErr error)
	RebindMode(harness domain.AgentHarness, model, effort string, mode domain.SessionMode) error
}

type autoSpawnAdmitterKey struct{}

// WithAutoSpawnAdmitter attaches an admission breaker hook to ctx.
func WithAutoSpawnAdmitter(ctx context.Context, admitter AutoSpawnAdmitter) context.Context {
	if admitter == nil {
		return ctx
	}
	return context.WithValue(ctx, autoSpawnAdmitterKey{}, admitter)
}

func autoSpawnAdmitterFrom(ctx context.Context) AutoSpawnAdmitter {
	admitter, _ := ctx.Value(autoSpawnAdmitterKey{}).(AutoSpawnAdmitter)
	return admitter
}

func automaticQuotaWorker(cfg ports.SpawnConfig) bool {
	return cfg.Kind == domain.KindWorker && cfg.ProjectID != "" &&
		(cfg.CreatedBy == domain.SessionCreatedByAutomation || cfg.AutomationRunID != nil || cfg.ParentSessionID != "") &&
		cfg.Harness == "" && cfg.AgentAccountID == "" && cfg.AgentConfig.Model == "" &&
		cfg.AgentConfig.Mode == "" && cfg.AgentConfig.Effort == "" && !cfg.EffortOverride && !cfg.AgentConfigResolved &&
		cfg.RequestedMode == ""
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
	admitter := autoSpawnAdmitterFrom(ctx)
	var selected workerquota.Target
	var effectiveMode domain.SessionMode
	var remaining []workerquota.Target
	var blocked bool
	var firstPauseErr error
	for i, route := range targets {
		if _, ok := workerquota.Select([]workerquota.Target{route}, windows, m.clock()); !ok {
			blocked = true
			continue
		}
		if (policy.Primary == nil && route == primary) || m.quotaTargetReady(ctx, route, project.Config, *cfg) {
			routeModel := route.Model
			if routeModel == "" {
				routeModel = defaults[route.Harness]
			}
			routeMode := m.effectiveQuotaRouteMode(ctx, route, project.Config, *cfg)
			if admitter != nil {
				admitted, pauseErr := admitter.TryAdmit(route.Harness, routeModel, route.Effort, routeMode)
				if !admitted {
					if firstPauseErr == nil {
						firstPauseErr = pauseErr
					}
					continue
				}
			}
			selected, effectiveMode, remaining = route, routeMode, targets[i+1:]
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if selected.Harness == "" {
		if firstPauseErr != nil {
			return nil, firstPauseErr
		}
		if blocked {
			return nil, workerquota.ErrDeferred
		}
		return nil, workerquota.ErrNoReadyRoute
	}
	if selected != primary {
		cfg.Harness = selected.Harness
		cfg.AgentConfig.Model, cfg.AgentConfig.Effort = selected.Model, selected.Effort
		cfg.EffortOverride = selected.Effort != ""
		// Remove provider-specific inherited tuning from this copy of the
		// project while preserving worker permissions and other project rules.
		project.Config.AgentConfig.Model, project.Config.AgentConfig.Effort, project.Config.AgentConfig.Mode = "", "", ""
		project.Config.Worker.AgentConfig.Model, project.Config.Worker.AgentConfig.Effort, project.Config.Worker.AgentConfig.Mode = "", "", ""
	}
	if selected.Mode != "" {
		cfg.RequestedMode = selected.Mode
	}
	if selected.Model == "" && defaults[selected.Harness] != "" {
		cfg.AgentConfig.Model = defaults[selected.Harness]
		cfg.AgentConfig.Effort = selected.Effort
	}
	if selected != primary || selected.Mode != "" {
		m.logger.Info("worker quota selected configured route",
			"project", cfg.ProjectID, "harness", selected.Harness,
			"model", cfg.AgentConfig.Model, "effort", selected.Effort, "mode", effectiveMode)
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
	if !target.Harness.SupportsMode(target.Mode) {
		return false
	}
	if target.Mode == domain.SessionModeTUI && m.validateRuntimePrerequisites() != nil {
		return false
	}
	if m.agentReadiness != nil {
		readiness, err := m.agentReadiness.EnsureAgentReadiness(ctx, string(target.Harness), domain.AgentReadinessPurposeLaunch)
		if err != nil || readiness.Installation.State == domain.AgentInstallationNotInstalled {
			return false
		}
		if readiness.Authentication.State == domain.AgentAuthenticationUnauthorized && readiness.Authentication.Freshness == domain.AgentReadinessFresh {
			if !(target.Harness == domain.HarnessAgy && target.Mode == domain.SessionModeTUI) {
				return false
			}
		}
	}
	if target.Mode == domain.SessionModeChat || (target.Mode == "" && (cfg.RequestedMode == domain.SessionModeChat || target.Harness.ChatOnly())) {
		if m.chat == nil || !m.chat.SupportsChat(target.Harness) {
			return false
		}
		targetCfg := cfg
		targetCfg.Harness = target.Harness
		permissions := m.quotaTargetPermissions(ctx, target, project, targetCfg)
		return m.preflightChat(ctx, target.Harness, permissions, m.chatPreflightEnv(targetCfg, project.Env)) == nil
	}
	return true
}

func (m *Manager) effectiveQuotaRouteMode(ctx context.Context, target workerquota.Target, project domain.ProjectConfig, cfg ports.SpawnConfig) domain.SessionMode {
	if target.Mode != "" {
		return target.Mode
	}
	if target.Harness.ChatOnly() {
		return domain.SessionModeChat
	}
	mode := m.resolveSessionMode(ctx, cfg.Kind, cfg.RequestedMode)
	if mode != domain.SessionModeChat {
		return mode
	}
	if m.chat == nil {
		return domain.SessionModeTUI
	}
	targetCfg := cfg
	targetCfg.Harness = target.Harness
	permissions := m.quotaTargetPermissions(ctx, target, project, targetCfg)
	if err := m.preflightChat(ctx, target.Harness, permissions, m.chatPreflightEnv(targetCfg, project.Env)); err != nil {
		fallbackAllowed := errors.Is(err, ports.ErrChatUnsupported) ||
			errors.Is(err, ports.ErrChatDriverUnavailable) ||
			errors.Is(err, ports.ErrChatDriverIncompatible) ||
			errors.Is(err, ports.ErrChatAuthRequired)
		if fallbackAllowed {
			return domain.SessionModeTUI
		}
	}
	return domain.SessionModeChat
}

func (m *Manager) quotaTargetPermissions(ctx context.Context, target workerquota.Target, project domain.ProjectConfig, cfg ports.SpawnConfig) ports.PermissionMode {
	permissions := applySpawnAgentConfig(effectiveAgentConfig(target.Harness, cfg.Kind, project), cfg.AgentConfig).Permissions
	if target.Harness == domain.HarnessUnreal && cfg.AgentConfig.Permissions == "" &&
		effectiveAgentConfig(target.Harness, cfg.Kind, project).Permissions == "" {
		return ports.PermissionModeBypassPermissions
	}
	if cfg.AgentConfig.Permissions == "" && effectiveAgentConfig(target.Harness, cfg.Kind, project).Permissions == "" && m.chat != nil {
		if catalog, ok := m.chat.(interface {
			PermissionModes(context.Context, domain.AgentHarness) ([]ports.PermissionMode, error)
		}); ok {
			if modes, err := catalog.PermissionModes(ctx, target.Harness); err == nil && len(modes) == 1 && modes[0] == ports.PermissionModeDefault {
				return ports.PermissionModeDefault
			}
		}
	}
	return permissions
}
