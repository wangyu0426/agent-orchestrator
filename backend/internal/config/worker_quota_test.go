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
		policy.Primary.Mode != "" || len(policy.Fallbacks) != 1 || policy.Fallbacks[0].Harness != domain.HarnessAgy ||
		policy.Fallbacks[0].Mode != "" {
		t.Fatalf("policy = %+v, %v", policy, err)
	}

	t.Setenv("AO_WORKER_QUOTA_PRIMARY", `{"harness":"codex","model":"primary","mode":"chat"}`)
	t.Setenv("AO_WORKER_QUOTA_FALLBACKS", `[{"harness":"agy","model":"gemini-4-hydrogen-low","mode":"tui"}]`)
	policy, err = loadWorkerQuota()
	if err != nil || policy.Primary.Mode != domain.SessionModeChat || policy.Fallbacks[0].Mode != domain.SessionModeTUI {
		t.Fatalf("mode overrides = %+v, %v", policy, err)
	}

	for _, tc := range []struct {
		name      string
		primary   string
		fallbacks string
	}{
		{name: "invalid fallback mode", fallbacks: `[{"harness":"agy","model":"gemini-4-hydrogen-low","mode":"acp"}]`},
		{name: "invalid primary mode", primary: `{"harness":"codex","mode":"terminal"}`, fallbacks: `[{"harness":"agy"}]`},
		{name: "non-canonical casing", fallbacks: `[{"harness":"agy","mode":"TUI"}]`},
		{name: "whitespace mode", fallbacks: `[{"harness":"agy","mode":" tui"}]`},
		{name: "chat-only harness with tui fallback", fallbacks: `[{"harness":"unreal-agent","mode":"tui"}]`},
		{name: "chat-only docker-agent with tui primary", primary: `{"harness":"docker-agent","mode":"tui"}`, fallbacks: `[{"harness":"agy"}]`},
		{name: "tui-only harness with chat fallback", fallbacks: `[{"harness":"aider","mode":"chat"}]`},
		{name: "tui-only harness with chat primary", primary: `{"harness":"aider","mode":"chat"}`, fallbacks: `[{"harness":"agy"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AO_WORKER_QUOTA_PRIMARY", tc.primary)
			t.Setenv("AO_WORKER_QUOTA_FALLBACKS", tc.fallbacks)
			if _, err := loadWorkerQuota(); err == nil {
				t.Fatalf("expected loadWorkerQuota error for %s", tc.name)
			}
		})
	}
}
