package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

func TestLoadRoleQuotasIndependentOptInAndSharedQuotum(t *testing.T) {
	sessionFile := filepath.Join(t.TempDir(), "quotum-session")
	if err := os.WriteFile(sessionFile, []byte("secret-cookie"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AO_WORKER_QUOTA_FAILOVER", "")
	t.Setenv("AO_REVIEWER_QUOTA_FAILOVER", "on")
	t.Setenv("AO_REVIEWER_QUOTA_PRIMARY", `{"harness":"codex","model":"codex-review","effort":"high"}`)
	t.Setenv("AO_REVIEWER_QUOTA_FALLBACKS", `[{"harness":"agy","model":"agy-review","mode":"tui"},{"harness":"codex","model":"codex-review","account":"acct-2"}]`)
	t.Setenv("AO_ORCHESTRATOR_QUOTA_FAILOVER", "on")
	t.Setenv("AO_ORCHESTRATOR_QUOTA_FALLBACKS", `[{"harness":"agy","model":"agy-orch","mode":"tui"}]`)
	t.Setenv("AO_QUOTUM_URL", "http://127.0.0.1:8080")
	t.Setenv("AO_QUOTUM_SESSION_FILE", sessionFile)
	t.Setenv("AO_QUOTUM_BINDINGS", `{"codex":{"sourceId":"s-codex"},"agy":{"sourceId":"s-agy"}}`)

	quotas, err := loadRoleQuotas()
	if err != nil {
		t.Fatalf("loadRoleQuotas: %v", err)
	}
	if quotas.Worker != nil {
		t.Fatalf("Worker policy = %+v, want nil when not opted in", quotas.Worker)
	}
	if quotas.Reviewer == nil || quotas.Reviewer.Role != workerquota.RoleReviewer {
		t.Fatalf("Reviewer policy = %+v, want RoleReviewer", quotas.Reviewer)
	}
	if len(quotas.Reviewer.Fallbacks) != 2 || quotas.Reviewer.Fallbacks[1].Account != "acct-2" {
		t.Fatalf("Reviewer fallbacks = %+v", quotas.Reviewer.Fallbacks)
	}
	if quotas.Orchestrator == nil || quotas.Orchestrator.Role != workerquota.RoleOrchestrator {
		t.Fatalf("Orchestrator policy = %+v, want RoleOrchestrator", quotas.Orchestrator)
	}
	if quotas.Reviewer.Source == nil || quotas.Reviewer.Source != quotas.Orchestrator.Source {
		t.Fatalf("Reviewer and Orchestrator must share the same Quotum Source instance")
	}
}

func TestLoadRoleQuotasRejectsInvalidReviewerAndUntrimmedAccount(t *testing.T) {
	t.Run("unknown reviewer harness", func(t *testing.T) {
		t.Setenv("AO_REVIEWER_QUOTA_FAILOVER", "on")
		t.Setenv("AO_REVIEWER_QUOTA_FALLBACKS", `[{"harness":"not-a-reviewer"}]`)
		if _, err := loadRoleQuotas(); err == nil {
			t.Fatal("expected error for unknown reviewer harness")
		}
	})
	t.Run("untrimmed account", func(t *testing.T) {
		t.Setenv("AO_ORCHESTRATOR_QUOTA_FAILOVER", "on")
		t.Setenv("AO_ORCHESTRATOR_QUOTA_FALLBACKS", `[{"harness":"agy","account":" acct-1 "}]`)
		if _, err := loadRoleQuotas(); err == nil {
			t.Fatal("expected error for untrimmed account")
		}
	})
	t.Run("valid reviewer-only harness", func(t *testing.T) {
		t.Setenv("AO_REVIEWER_QUOTA_FAILOVER", "on")
		t.Setenv("AO_REVIEWER_QUOTA_FALLBACKS", `[{"harness":"`+string(domain.ReviewerAgy)+`","mode":"tui"}]`)
		quotas, err := loadRoleQuotas()
		if err != nil || quotas.Reviewer == nil {
			t.Fatalf("loadRoleQuotas = %+v, err=%v", quotas, err)
		}
	})
}
