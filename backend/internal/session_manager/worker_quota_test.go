package sessionmanager

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

type testQuotaSource struct {
	now     time.Time
	blocked map[domain.AgentHarness]bool
	reads   int
}

func (q *testQuotaSource) Read(_ context.Context, targets []workerquota.Target) (map[workerquota.Target][]workerquota.Window, error) {
	q.reads++
	out := make(map[workerquota.Target][]workerquota.Window)
	for _, target := range targets {
		out[target] = []workerquota.Window{{ObservedAt: q.now, ValidUntil: q.now.Add(time.Hour), Exhausted: q.blocked[target.Harness]}}
	}
	return out, nil
}

func TestWorkerQuotaSpawnPreservesExplicitOverrides(t *testing.T) {
	now := time.Now()
	source := &testQuotaSource{now: now, blocked: map[domain.AgentHarness]bool{domain.HarnessCodex: true}}
	manager := &Manager{clock: func() time.Time { return now }, logger: slog.New(slog.DiscardHandler), agents: fakeAgents{}}
	project := domain.ProjectConfig{Worker: domain.RoleOverride{Harness: domain.HarnessCodex, AgentConfig: domain.AgentConfig{
		Model: "codex-model", Effort: "high", Permissions: domain.PermissionModeDefault,
	}}}
	automatic := ports.SpawnConfig{Kind: domain.KindWorker, ProjectID: "project", IssueID: "github:org/repo#14", CreatedBy: domain.SessionCreatedByAutomation}
	apply := func(cfg ports.SpawnConfig) (ports.SpawnConfig, domain.ProjectConfig, []workerquota.Target, error) {
		settings := project
		remaining, err := manager.selectWorkerQuotaTarget(t.Context(), &cfg, &settings)
		return cfg, settings, remaining, err
	}
	if cfg, _, remaining, err := apply(automatic); err != nil || remaining != nil || cfg.Harness != automatic.Harness || cfg.IssueID != automatic.IssueID {
		t.Fatalf("disabled policy changed spawn: %+v, %v, %v", cfg, remaining, err)
	}
	agy := workerquota.Target{Harness: domain.HarnessAgy, Model: "gemini-pro"}
	codexFallback := workerquota.Target{Harness: domain.HarnessCodex, Model: "codex-fallback", Effort: "low"}
	manager.workerQuota = &workerquota.Policy{Fallbacks: []workerquota.Target{agy, codexFallback, agy}, Source: source}
	explicit := automatic
	explicit.AgentConfig.Model = "chosen-model"
	if cfg, _, remaining, err := apply(explicit); err != nil || remaining != nil || cfg.AgentConfig.Model != "chosen-model" || source.reads != 0 {
		t.Fatalf("override was routed: %+v, %v, %v", cfg, remaining, err)
	}
	cfg, settings, remaining, err := apply(automatic)
	if err != nil || !slices.Equal(remaining, []workerquota.Target{codexFallback}) || cfg.Harness != domain.HarnessAgy || cfg.AgentConfig.Model != "gemini-pro" ||
		cfg.IssueID != automatic.IssueID || settings.Worker.AgentConfig.Permissions != domain.PermissionModeDefault ||
		settings.Worker.AgentConfig.Effort != "" {
		t.Fatalf("incorrect fallback: %+v, %+v, %v, %v", cfg, settings.Worker, remaining, err)
	}
	source.blocked[domain.HarnessAgy] = true
	if _, _, remaining, err := apply(automatic); remaining != nil || !errors.Is(err, workerquota.ErrDeferred) {
		t.Fatalf("all exhausted = %v, %v", remaining, err)
	}
	delete(source.blocked, domain.HarnessCodex)
	if cfg, _, remaining, err := apply(automatic); err != nil || !slices.Equal(remaining, []workerquota.Target{agy, codexFallback}) || cfg.Harness != "" {
		t.Fatalf("primary not restored: %+v, %v, %v", cfg, remaining, err)
	}
}
