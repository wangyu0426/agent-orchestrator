package config

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestLoadProjectDefaultsWorkerPolicy(t *testing.T) {
	t.Run("valid worker permissions and sandbox", func(t *testing.T) {
		t.Setenv("AO_DEFAULT_WORKER_PERMISSIONS", "bypass-permissions")
		t.Setenv("AO_DEFAULT_WORKER_SANDBOX", "restricted")

		d, err := loadProjectDefaults()
		if err != nil {
			t.Fatalf("loadProjectDefaults() error = %v", err)
		}
		if d.WorkerPermissions != domain.PermissionModeBypassPermissions {
			t.Errorf("WorkerPermissions = %q, want bypass-permissions", d.WorkerPermissions)
		}
		if d.WorkerSandbox != domain.SandboxModeRestricted {
			t.Errorf("WorkerSandbox = %q, want restricted", d.WorkerSandbox)
		}
	})

	t.Run("invalid worker permissions rejected", func(t *testing.T) {
		t.Setenv("AO_DEFAULT_WORKER_PERMISSIONS", "yolo")
		if _, err := loadProjectDefaults(); err == nil {
			t.Fatal("expected error for invalid AO_DEFAULT_WORKER_PERMISSIONS")
		}
	})

	t.Run("invalid worker sandbox rejected", func(t *testing.T) {
		t.Setenv("AO_DEFAULT_WORKER_SANDBOX", "none")
		if _, err := loadProjectDefaults(); err == nil {
			t.Fatal("expected error for invalid AO_DEFAULT_WORKER_SANDBOX")
		}
	})
}
