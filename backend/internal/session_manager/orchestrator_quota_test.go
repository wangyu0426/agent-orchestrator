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

type staticQuotaWindows map[workerquota.Target][]workerquota.Window

func (s staticQuotaWindows) Read(_ context.Context, targets []workerquota.Target) (map[workerquota.Target][]workerquota.Window, error) {
	out := make(map[workerquota.Target][]workerquota.Window, len(targets))
	for _, t := range targets {
		out[t] = s[t]
	}
	return out, nil
}

func TestOrchestratorQuotaFailoverIndependentOfWorkerPolicy(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	codex := workerquota.Target{Harness: domain.HarnessCodex, Model: "codex-orch", Effort: "high"}
	agy := workerquota.Target{Harness: domain.HarnessAgy, Model: "agy-orch-pro", Effort: "high", Mode: domain.SessionModeTUI, Account: "acct-orch"}
	source := staticQuotaWindows{
		codex: {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), ResetAt: now.Add(2 * time.Hour), Exhausted: true}},
	}
	manager := &Manager{
		clock:  func() time.Time { return now },
		logger: slog.New(slog.DiscardHandler),
		agents: fakeAgents{},
		orchestratorQuota: &workerquota.Policy{
			Role:      workerquota.RoleOrchestrator,
			Primary:   &codex,
			Fallbacks: []workerquota.Target{agy},
			Source:    source,
		},
	}

	project := domain.ProjectRecord{
		ID: "mer",
		Config: domain.ProjectConfig{
			Orchestrator: domain.RoleOverride{
				Harness:     domain.HarnessCodex,
				AgentConfig: domain.AgentConfig{Model: "codex-orch", Effort: "high"},
			},
		},
	}

	// Automatic orchestrator spawn fails over to AGY TUI + account binding.
	orchCfg := ports.SpawnConfig{
		ProjectID: "mer",
		Kind:      domain.KindOrchestrator,
		CreatedBy: domain.SessionCreatedByAutomation,
	}
	remaining, err := manager.selectWorkerQuotaTarget(t.Context(), &orchCfg, &project)
	if err != nil {
		t.Fatalf("selectWorkerQuotaTarget(orchestrator): %v", err)
	}
	if remaining == nil || orchCfg.Harness != domain.HarnessAgy || orchCfg.AgentConfig.Model != "agy-orch-pro" ||
		orchCfg.RequestedMode != domain.SessionModeTUI || orchCfg.AgentAccountID != "acct-orch" {
		t.Fatalf("orchestrator cfg = %+v, remaining=%v", orchCfg, remaining)
	}

	// Worker spawn is untouched because workerQuota is nil.
	workerCfg := ports.SpawnConfig{
		ProjectID: "mer",
		Kind:      domain.KindWorker,
		CreatedBy: domain.SessionCreatedByAutomation,
	}
	workerRem, err := manager.selectWorkerQuotaTarget(t.Context(), &workerCfg, &project)
	if err != nil || workerRem != nil || workerCfg.Harness != "" {
		t.Fatalf("worker cfg should be untouched when workerQuota is nil: cfg=%+v rem=%v err=%v", workerCfg, workerRem, err)
	}

	// Explicit manual orchestrator spawn bypasses orchestrator quota failover.
	manualOrch := ports.SpawnConfig{
		ProjectID: "mer",
		Kind:      domain.KindOrchestrator,
	}
	manualRem, err := manager.selectWorkerQuotaTarget(t.Context(), &manualOrch, &project)
	if err != nil || manualRem != nil || manualOrch.Harness != "" {
		t.Fatalf("manual orchestrator should bypass quota failover: cfg=%+v rem=%v err=%v", manualOrch, manualRem, err)
	}
}

func TestOrchestratorQuotaAllExhaustedReturnsDeferredWithResetAt(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(45 * time.Minute)
	codex := workerquota.Target{Harness: domain.HarnessCodex, Model: "codex-orch"}
	agy := workerquota.Target{Harness: domain.HarnessAgy, Model: "agy-orch", Mode: domain.SessionModeTUI}
	manager := &Manager{
		clock:  func() time.Time { return now },
		logger: slog.New(slog.DiscardHandler),
		agents: fakeAgents{},
		orchestratorQuota: &workerquota.Policy{
			Role:      workerquota.RoleOrchestrator,
			Primary:   &codex,
			Fallbacks: []workerquota.Target{agy},
			Source: staticQuotaWindows{
				codex: {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(2 * time.Hour), ResetAt: resetAt, Exhausted: true}},
				agy:   {{ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(2 * time.Hour), ResetAt: now.Add(2 * time.Hour), Exhausted: true}},
			},
		},
	}
	project := domain.ProjectRecord{ID: "mer", Config: domain.ProjectConfig{Orchestrator: domain.RoleOverride{Harness: domain.HarnessCodex}}}
	cfg := ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindOrchestrator, CreatedBy: domain.SessionCreatedByAutomation}
	_, err := manager.selectWorkerQuotaTarget(t.Context(), &cfg, &project)
	if !errors.Is(err, workerquota.ErrDeferred) {
		t.Fatalf("err = %v, want ErrDeferred", err)
	}
	var deferred *workerquota.DeferredError
	if !errors.As(err, &deferred) || deferred.Role != workerquota.RoleOrchestrator || !deferred.RetryAt.Equal(resetAt) {
		t.Fatalf("DeferredError = %+v, want role=orchestrator retryAt=%v", deferred, resetAt)
	}
}
