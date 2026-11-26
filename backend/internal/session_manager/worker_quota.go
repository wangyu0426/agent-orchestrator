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

func automaticQuotaSpawn(cfg ports.SpawnConfig) (workerquota.Role, bool) {
	if cfg.ProjectID == "" ||
		(!cfg.AllowQuotaFailover && cfg.CreatedBy != domain.SessionCreatedByAutomation && cfg.AutomationRunID == nil && cfg.ParentSessionID == "") ||
		cfg.Harness != "" || cfg.AgentAccountID != "" || cfg.AgentConfig.Model != "" ||
		cfg.AgentConfig.Mode != "" || cfg.AgentConfig.Effort != "" || cfg.EffortOverride || cfg.AgentConfigResolved ||
		cfg.RequestedMode != "" {
		return "", false
	}
	switch cfg.Kind {
	case domain.KindWorker:
		return workerquota.RoleWorker, true
	case domain.KindOrchestrator:
		return workerquota.RoleOrchestrator, true
	default:
		return "", false
	}
}

func (m *Manager) quotaPolicyForRole(role workerquota.Role) *workerquota.Policy {
	switch role {
	case workerquota.RoleWorker:
		return m.workerQuota
	case workerquota.RoleOrchestrator:
		return m.orchestratorQuota
	default:
		return nil
	}
}

type quotaSelection struct {
	active           bool
	remaining        []workerquota.Target
	mode             domain.SessionMode
	chatPreflighted  bool
	chatPreflightErr error
}

func (m *Manager) selectWorkerQuotaTarget(ctx context.Context, cfg *ports.SpawnConfig, project *domain.ProjectRecord) ([]workerquota.Target, error) {
	sel, err := m.selectWorkerQuota(ctx, cfg, project)
	if err != nil {
		return nil, err
	}
	return sel.remaining, nil
}

func (m *Manager) selectWorkerQuota(ctx context.Context, cfg *ports.SpawnConfig, project *domain.ProjectRecord) (quotaSelection, error) {
	role, ok := automaticQuotaSpawn(*cfg)
	if !ok {
		return quotaSelection{}, nil
	}
	policy := m.quotaPolicyForRole(role)
	if policy == nil {
		return quotaSelection{}, nil
	}
	harness := effectiveHarnessWithDefaults("", cfg.Kind, project.Config, m.projectDefaults)
	base := effectiveAgentConfigWithDefaults(harness, cfg.Kind, project.Config, m.projectDefaults)
	primary := workerquota.Target{Harness: harness, Model: base.Model, Effort: base.Effort}
	targets := policy.Targets(primary)
	if len(targets) == 0 {
		return quotaSelection{}, nil
	}
	defaults := policy.DefaultModels(ctx, *project, targets)
	windows := m.roleQuotaWindows(ctx, policy, targets, defaults)
	admitter := autoSpawnAdmitterFrom(ctx)
	var selected workerquota.Target
	var selectedMode domain.SessionMode
	var selectedPreflighted bool
	var selectedPreflightErr error
	var remaining []workerquota.Target
	var blocked bool
	var firstPauseErr error
	for i, route := range targets {
		if _, ok := workerquota.Select([]workerquota.Target{route}, windows, m.clock()); !ok {
			blocked = true
			continue
		}
		bypassReady := policy.Primary == nil && route == primary
		ready, preflighted := true, false
		if !bypassReady {
			ready, preflighted = m.quotaTargetReady(ctx, route, project.Config, *cfg)
		}
		if !ready {
			continue
		}
		routeModel := route.Model
		if routeModel == "" {
			routeModel = defaults[route.Harness]
		}
		var routeMode domain.SessionMode
		var preflightErr error
		if preflighted {
			routeMode = domain.SessionModeChat
		} else {
			routeMode, preflighted, preflightErr = m.resolveQuotaRouteMode(ctx, route, project.Config, *cfg)
		}
		if admitter != nil {
			admitted, pauseErr := admitter.TryAdmit(route.Harness, routeModel, route.Effort, routeMode)
			if !admitted {
				if firstPauseErr == nil {
					firstPauseErr = pauseErr
				}
				continue
			}
		}
		selected, selectedMode, selectedPreflighted, selectedPreflightErr, remaining = route, routeMode, preflighted, preflightErr, targets[i+1:]
		break
	}
	if err := ctx.Err(); err != nil {
		return quotaSelection{}, err
	}
	if selected.Harness == "" {
		if firstPauseErr != nil {
			return quotaSelection{}, firstPauseErr
		}
		if blocked {
			retryAt, _ := workerquota.NextResetAt(targets, windows, m.clock())
			return quotaSelection{}, &workerquota.DeferredError{Role: role, RetryAt: retryAt}
		}
		return quotaSelection{}, workerquota.ErrNoReadyRoute
	}
	if selected != primary {
		cfg.Harness = selected.Harness
		cfg.AgentConfig.Model, cfg.AgentConfig.Effort = selected.Model, selected.Effort
		cfg.EffortOverride = selected.Effort != ""
		// Remove provider-specific inherited tuning from this copy of the
		// project while preserving role permissions and other project rules.
		project.Config.AgentConfig.Model, project.Config.AgentConfig.Effort, project.Config.AgentConfig.Mode = "", "", ""
		if cfg.Kind == domain.KindOrchestrator {
			project.Config.Orchestrator.AgentConfig.Model, project.Config.Orchestrator.AgentConfig.Effort, project.Config.Orchestrator.AgentConfig.Mode = "", "", ""
		} else {
			project.Config.Worker.AgentConfig.Model, project.Config.Worker.AgentConfig.Effort, project.Config.Worker.AgentConfig.Mode = "", "", ""
		}
	}
	if selected.Account != "" {
		cfg.AgentAccountID = selected.Account
	}
	if selected.Mode != "" {
		cfg.RequestedMode = selected.Mode
	}
	if selected.Model == "" && defaults[selected.Harness] != "" {
		cfg.AgentConfig.Model = defaults[selected.Harness]
		cfg.AgentConfig.Effort = selected.Effort
	}
	if selected != primary || selected.Mode != "" || selected.Account != "" {
		m.logger.Info("worker quota selected configured route",
			"role", role, "project", cfg.ProjectID, "harness", selected.Harness,
			"model", cfg.AgentConfig.Model, "effort", selected.Effort, "mode", selectedMode, "account", selected.Account)
	}
	return quotaSelection{
		active:           true,
		remaining:        remaining,
		mode:             selectedMode,
		chatPreflighted:  selectedPreflighted,
		chatPreflightErr: selectedPreflightErr,
	}, nil
}

func (m *Manager) roleQuotaWindows(ctx context.Context, policy *workerquota.Policy, targets []workerquota.Target, defaults map[domain.AgentHarness]string) map[workerquota.Target][]workerquota.Window {
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
	windows, err := policy.Windows(ctx, targets, failures, now, defaults)
	if err != nil {
		m.logger.Debug("worker quota source unavailable", "error", err)
	}
	return windows
}

func (m *Manager) quotaTargetReady(ctx context.Context, target workerquota.Target, project domain.ProjectConfig, cfg ports.SpawnConfig) (ready, chatPreflighted bool) {
	if _, ok := m.agents.Agent(target.Harness); !ok || validateSpawnModel(target.Harness, target.Model) != nil {
		return false, false
	}
	effectiveSandbox := applySpawnAgentConfig(effectiveAgentConfigWithDefaults(target.Harness, cfg.Kind, project, m.projectDefaults), cfg.AgentConfig).Sandbox
	if validateSpawnSandbox(target.Harness, effectiveSandbox) != nil {
		return false, false
	}
	if !target.Harness.SupportsMode(target.Mode) {
		return false, false
	}
	if target.Mode == domain.SessionModeTUI && m.validateRuntimePrerequisites() != nil {
		return false, false
	}
	if m.agentReadiness != nil {
		readiness, err := m.agentReadiness.EnsureAgentReadiness(ctx, string(target.Harness), domain.AgentReadinessPurposeLaunch)
		if err != nil || readiness.Installation.State == domain.AgentInstallationNotInstalled {
			return false, false
		}
		if readiness.Authentication.State == domain.AgentAuthenticationUnauthorized && readiness.Authentication.Freshness == domain.AgentReadinessFresh {
			if target.Harness != domain.HarnessAgy || target.Mode != domain.SessionModeTUI {
				return false, false
			}
		}
	}
	if target.Mode == domain.SessionModeChat || (target.Mode == "" && (cfg.RequestedMode == domain.SessionModeChat || target.Harness.ChatOnly())) {
		if m.chat == nil || !m.chat.SupportsChat(target.Harness) {
			return false, false
		}
		targetCfg := cfg
		targetCfg.Harness = target.Harness
		permissions := m.quotaTargetPermissions(ctx, target, project, targetCfg)
		return m.preflightChat(ctx, target.Harness, permissions, m.chatPreflightEnv(targetCfg, project.Env)) == nil, true
	}
	return true, false
}

func (m *Manager) resolveQuotaRouteMode(ctx context.Context, target workerquota.Target, project domain.ProjectConfig, cfg ports.SpawnConfig) (mode domain.SessionMode, chatPreflighted bool, preflightErr error) {
	if target.Mode != "" {
		return target.Mode, false, nil
	}
	if target.Harness.ChatOnly() {
		return domain.SessionModeChat, false, nil
	}
	mode = m.resolveSessionMode(ctx, cfg.Kind, cfg.RequestedMode)
	if mode != domain.SessionModeChat {
		return mode, false, nil
	}
	if m.chat == nil {
		return domain.SessionModeTUI, true, nil
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
			return domain.SessionModeTUI, true, nil
		}
		return domain.SessionModeChat, true, err
	}
	return domain.SessionModeChat, true, nil
}

func (m *Manager) quotaTargetPermissions(ctx context.Context, target workerquota.Target, project domain.ProjectConfig, cfg ports.SpawnConfig) ports.PermissionMode {
	base := effectiveAgentConfigWithDefaults(target.Harness, cfg.Kind, project, m.projectDefaults)
	permissions := applySpawnAgentConfig(base, cfg.AgentConfig).Permissions
	if target.Harness == domain.HarnessUnreal && cfg.AgentConfig.Permissions == "" &&
		base.Permissions == "" {
		return ports.PermissionModeBypassPermissions
	}
	if cfg.AgentConfig.Permissions == "" && base.Permissions == "" && m.chat != nil {
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
