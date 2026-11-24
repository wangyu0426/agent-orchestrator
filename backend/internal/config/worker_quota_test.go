package config

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestWorkerQuotaConfigIsOptIn(t *testing.T) {
	t.Setenv("AO_WORKER_QUOTA_FAILOVER", "")
	t.Setenv("AO_WORKER_QUOTA_FALLBACKS", "not JSON")
	if policy, err := loadWorkerQuota(); policy != nil || err != nil {
		t.Fatalf("disabled policy = %+v, %v", policy, err)
	}
	t.Setenv("AO_WORKER_QUOTA_FAILOVER", "on")
	if _, err := loadWorkerQuota(); err == nil {
		t.Fatal("accepted invalid fallback configuration")
	}
	t.Setenv("AO_WORKER_QUOTA_FALLBACKS", `[{"harness":"agy","model":"gemini-pro"}]`)
	t.Setenv("AO_WORKER_QUOTA_PRIMARY", `{"harness":"codex","model":"primary","effort":"high"}`)
	t.Setenv("AO_QUOTUM_URL", "")
	policy, err := loadWorkerQuota()
	if err != nil || policy.Primary.Harness != domain.HarnessCodex || policy.Primary.Effort != "high" ||
		len(policy.Fallbacks) != 1 || policy.Fallbacks[0].Harness != domain.HarnessAgy {
		t.Fatalf("policy = %+v, %v", policy, err)
	}
}
