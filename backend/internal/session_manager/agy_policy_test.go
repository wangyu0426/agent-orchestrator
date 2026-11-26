package sessionmanager

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

func TestWorkerPolicyPrecedenceAndRestore(t *testing.T) {
	defaults := domain.ProjectDefaults{
		WorkerPermissions: domain.PermissionModeBypassPermissions,
		WorkerSandbox:     domain.SandboxModeRestricted,
	}

	t.Run("daemon global default applies to new worker when project unset", func(t *testing.T) {
		eff := effectiveAgentConfigWithDefaults(domain.HarnessAgy, domain.KindWorker, domain.ProjectConfig{}, defaults)
		resolved := applySpawnAgentConfig(eff, ports.AgentConfig{})
		if resolved.Permissions != ports.PermissionModeBypassPermissions || resolved.Sandbox != ports.SandboxModeRestricted {
			t.Fatalf("resolved = %+v, want bypass-permissions + restricted", resolved)
		}
	})

	t.Run("project config overrides daemon global default", func(t *testing.T) {
		project := domain.ProjectConfig{
			AgentConfig: domain.AgentConfig{
				Permissions: domain.PermissionModeAuto,
				Sandbox:     domain.SandboxModeDefault,
			},
		}
		eff := effectiveAgentConfigWithDefaults(domain.HarnessAgy, domain.KindWorker, project, defaults)
		resolved := applySpawnAgentConfig(eff, ports.AgentConfig{})
		if resolved.Permissions != ports.PermissionModeAuto || resolved.Sandbox != ports.SandboxModeDefault {
			t.Fatalf("resolved = %+v, want auto + default", resolved)
		}
	})

	t.Run("per-spawn override wins over project and global", func(t *testing.T) {
		project := domain.ProjectConfig{
			Worker: domain.RoleOverride{
				AgentConfig: domain.AgentConfig{
					Permissions: domain.PermissionModeAcceptEdits,
					Sandbox:     domain.SandboxModeRestricted,
				},
			},
		}
		eff := effectiveAgentConfigWithDefaults(domain.HarnessAgy, domain.KindWorker, project, defaults)
		resolved := applySpawnAgentConfig(eff, ports.AgentConfig{
			Permissions: ports.PermissionModeBypassPermissions,
			Sandbox:     ports.SandboxModeUnrestricted,
		})
		if resolved.Permissions != ports.PermissionModeBypassPermissions || resolved.Sandbox != ports.SandboxModeUnrestricted {
			t.Fatalf("resolved = %+v, want bypass-permissions + unrestricted", resolved)
		}
	})

	t.Run("seedRecordWithDefaults persists resolved sandbox and permissions", func(t *testing.T) {
		rec := seedRecordWithDefaults(ports.SpawnConfig{
			ProjectID: "proj-1",
			Kind:      domain.KindWorker,
			Harness:   domain.HarnessAgy,
		}, domain.ProjectConfig{}, defaults, time.Now())
		if rec.Metadata.Permissions != domain.PermissionModeBypassPermissions || rec.Metadata.Sandbox != domain.SandboxModeRestricted {
			t.Fatalf("metadata = %+v, want bypass-permissions + restricted", rec.Metadata)
		}

		restored := restoredAgentConfig(rec, domain.ProjectConfig{})
		if restored.Permissions != domain.PermissionModeBypassPermissions || restored.Sandbox != domain.SandboxModeRestricted {
			t.Fatalf("restored = %+v, want bypass-permissions + restricted", restored)
		}

		legacyCodex := domain.SessionRecord{Harness: domain.HarnessCodex, Kind: domain.KindWorker}
		restoredLegacy := restoredAgentConfig(legacyCodex, domain.ProjectConfig{
			AgentConfig: domain.AgentConfig{Sandbox: domain.SandboxModeRestricted},
		})
		if restoredLegacy.Sandbox != domain.SandboxModeDefault {
			t.Fatalf("legacy restored sandbox = %q, want default", restoredLegacy.Sandbox)
		}
	})

	t.Run("reviewer never inherits worker permissions or sandbox", func(t *testing.T) {
		project := domain.ProjectConfig{
			Worker: domain.RoleOverride{
				Harness: domain.HarnessClaudeCode,
				AgentConfig: domain.AgentConfig{
					Model:       "sonnet",
					Permissions: domain.PermissionModeBypassPermissions,
					Sandbox:     domain.SandboxModeUnrestricted,
				},
			},
		}
		_, revCfg := project.DefaultWorkerReviewer()
		if revCfg.Permissions != "" || revCfg.Sandbox != "" {
			t.Fatalf("reviewer inherited worker policy: %+v", revCfg)
		}
	})

	t.Run("resolveAgentConfig carries daemon worker defaults into Chat launch and matches persisted metadata", func(t *testing.T) {
		m := &Manager{}
		m.SetProjectDefaults(defaults)
		spawnCfg := ports.SpawnConfig{
			ProjectID:     "proj-1",
			Kind:          domain.KindWorker,
			Harness:       domain.HarnessAgy,
			RequestedMode: domain.SessionModeChat,
		}
		resolved, err := m.resolveAgentConfig(context.Background(), spawnCfg, domain.ProjectConfig{})
		if err != nil {
			t.Fatalf("resolveAgentConfig() error = %v", err)
		}
		seed := seedRecordWithDefaults(spawnCfg, domain.ProjectConfig{}, m.projectDefaults, time.Now())
		if resolved.Permissions != domain.PermissionModeBypassPermissions || resolved.Sandbox != domain.SandboxModeRestricted {
			t.Fatalf("resolved = %+v, want bypass-permissions + restricted", resolved)
		}
		if seed.Metadata.Permissions != resolved.Permissions || seed.Metadata.Sandbox != resolved.Sandbox {
			t.Fatalf("seed metadata (%+v) != resolved (%+v)", seed.Metadata, resolved)
		}
	})

	t.Run("daemon WorkerSandbox scopes to AGY and explicit non-default sandbox fails closed on other harnesses", func(t *testing.T) {
		effCodex := effectiveAgentConfigWithDefaults(domain.HarnessCodex, domain.KindWorker, domain.ProjectConfig{}, defaults)
		if effCodex.Sandbox != "" {
			t.Fatalf("codex inherited AGY-only WorkerSandbox: %q", effCodex.Sandbox)
		}
		if err := validateSpawnSandbox(domain.HarnessCodex, domain.SandboxModeDefault); err != nil {
			t.Fatalf("default sandbox on codex should be allowed, got %v", err)
		}
		if err := validateSpawnSandbox(domain.HarnessCodex, domain.SandboxModeRestricted); err == nil {
			t.Fatal("expected error for explicit restricted sandbox on codex")
		}
		if err := validateSpawnSandbox(domain.HarnessAgy, domain.SandboxModeRestricted); err != nil {
			t.Fatalf("restricted sandbox on agy should be allowed, got %v", err)
		}
	})

	t.Run("quota failover skips sandbox-incompatible fallback and selects later compatible route", func(t *testing.T) {
		now := time.Now()
		primary := workerquota.Target{Harness: domain.HarnessAgy, Model: "gemini-4-neon-low", Mode: domain.SessionModeTUI}
		codexFallback := workerquota.Target{Harness: domain.HarnessCodex, Model: "gpt-5", Mode: domain.SessionModeTUI}
		agyFallback := workerquota.Target{Harness: domain.HarnessAgy, Model: "gemini-4-hydrogen-low", Mode: domain.SessionModeTUI}

		policy := &workerquota.Policy{
			Role:      workerquota.RoleWorker,
			Primary:   &primary,
			Fallbacks: []workerquota.Target{codexFallback, agyFallback},
			Source: staticQuotaSource{windows: map[workerquota.Target][]workerquota.Window{
				primary: {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), Exhausted: true}},
			}},
		}
		m := &Manager{
			agents:      stubAgentRegistry{domain.HarnessAgy: nil, domain.HarnessCodex: nil},
			workerQuota: policy,
			clock:       func() time.Time { return now },
			logger:      slog.Default(),
		}
		project := domain.ProjectRecord{
			ID: "proj-1",
			Config: domain.ProjectConfig{
				AgentConfig: domain.AgentConfig{Sandbox: domain.SandboxModeRestricted},
			},
		}
		cfg := ports.SpawnConfig{
			ProjectID:          "proj-1",
			Kind:               domain.KindWorker,
			IssueID:            "gh#54",
			AllowQuotaFailover: true,
		}
		if _, err := m.selectWorkerQuota(context.Background(), &cfg, &project); err != nil {
			t.Fatalf("selectWorkerQuota() error = %v", err)
		}
		if cfg.Harness != agyFallback.Harness || cfg.AgentConfig.Model != agyFallback.Model {
			t.Fatalf("selected = (%q, %q), want later compatible AGY fallback (%q, %q)", cfg.Harness, cfg.AgentConfig.Model, agyFallback.Harness, agyFallback.Model)
		}

		// When only the exhausted AGY primary is compatible and the Codex fallback is incompatible,
		// defer rather than selecting the incompatible Codex fallback.
		policy.Fallbacks = []workerquota.Target{codexFallback}
		noRouteCfg := ports.SpawnConfig{
			ProjectID:          "proj-1",
			Kind:               domain.KindWorker,
			IssueID:            "gh#54",
			AllowQuotaFailover: true,
		}
		_, err := m.selectWorkerQuota(context.Background(), &noRouteCfg, &project)
		if !errors.Is(err, workerquota.ErrDeferred) {
			t.Fatalf("err = %v, want workerquota.ErrDeferred", err)
		}

		// When no route at all is sandbox-compatible, return ErrNoReadyRoute.
		policy.Primary = &codexFallback
		noCompatCfg := ports.SpawnConfig{
			ProjectID:          "proj-1",
			Kind:               domain.KindWorker,
			IssueID:            "gh#54",
			AllowQuotaFailover: true,
		}
		_, err = m.selectWorkerQuota(context.Background(), &noCompatCfg, &project)
		if !errors.Is(err, workerquota.ErrNoReadyRoute) {
			t.Fatalf("err = %v, want workerquota.ErrNoReadyRoute", err)
		}

		// Implicit project primary (policy.Primary == nil) that is sandbox-incompatible
		// must also be skipped before bypassReady so a ready compatible AGY fallback is selected.
		implicitPolicy := &workerquota.Policy{
			Role:      workerquota.RoleWorker,
			Primary:   nil,
			Fallbacks: []workerquota.Target{agyFallback},
		}
		m.workerQuota = implicitPolicy
		implicitProject := domain.ProjectRecord{
			ID: "proj-1",
			Config: domain.ProjectConfig{
				AgentConfig: domain.AgentConfig{Sandbox: domain.SandboxModeRestricted},
				Worker:      domain.RoleOverride{Harness: domain.HarnessCodex},
			},
		}
		implicitCfg := ports.SpawnConfig{
			ProjectID:          "proj-1",
			Kind:               domain.KindWorker,
			IssueID:            "gh#54",
			AllowQuotaFailover: true,
		}
		if _, err := m.selectWorkerQuota(context.Background(), &implicitCfg, &implicitProject); err != nil {
			t.Fatalf("implicit primary selectWorkerQuota() error = %v", err)
		}
		if implicitCfg.Harness != agyFallback.Harness || implicitCfg.AgentConfig.Model != agyFallback.Model {
			t.Fatalf("implicit selected = (%q, %q), want AGY fallback (%q, %q)", implicitCfg.Harness, implicitCfg.AgentConfig.Model, agyFallback.Harness, agyFallback.Model)
		}
	})
}

type staticQuotaSource struct {
	windows map[workerquota.Target][]workerquota.Window
}

func (s staticQuotaSource) Read(_ context.Context, _ []workerquota.Target) (map[workerquota.Target][]workerquota.Window, error) {
	return s.windows, nil
}

type stubAgentRegistry map[domain.AgentHarness]ports.Agent

func (r stubAgentRegistry) Agent(h domain.AgentHarness) (ports.Agent, bool) {
	a, ok := r[h]
	return a, ok
}
