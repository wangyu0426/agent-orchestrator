package sessionmanager

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
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
}
