package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

type quotaTerminalParker interface {
	ParkQuotaTUI(context.Context, domain.SessionRecord, time.Time) (bool, error)
}

type workerQuotaChat interface {
	QuotaFailure(context.Context, domain.SessionRecord) (domain.ConversationTurn, error)
	SupportsChat(domain.AgentHarness) bool
	QuotaTargetReady(context.Context, domain.SessionID, domain.AgentHarness) bool
	SwitchProviderAndSend(context.Context, domain.SessionID, domain.ProviderSwitchRequest, ports.ChatUserMessage) (domain.ConversationTurn, bool, error)
	ParkQuotaChat(context.Context, domain.SessionRecord, string) (bool, error)
}

type workerQuotaSweep struct {
	policy   *workerquota.Policy
	store    *sqlite.Store
	chat     workerQuotaChat
	terminal quotaTerminalParker
	agents   ports.AgentResolver
	accounts ports.AgentAccountLaunchEnv
	log      *slog.Logger
}

func (s workerQuotaSweep) poll(ctx context.Context) error {
	managed, err := s.store.WorkerQuotaSessions(ctx)
	if err != nil {
		return err
	}
	for _, worker := range managed {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.reconcile(ctx, worker); err != nil && ctx.Err() == nil {
			s.log.Warn("worker quota reconciliation failed", "session", worker.ID, "error", err)
		}
	}
	return nil
}

func (s workerQuotaSweep) reconcile(ctx context.Context, worker workerquota.Session) error {
	rec, found, err := s.store.GetSession(ctx, worker.ID)
	if err != nil || !found || rec.IsTerminated || rec.Kind != domain.KindWorker ||
		rec.Activity.State != domain.ActivityIdle || rec.ProvisionState.WithDefault() != domain.SessionProvisionReady {
		return err
	}
	current := worker.Initial
	if worker.Target.Harness == rec.Harness {
		current = worker.Target
	}
	// An empty admitted model uses the provider default. A later nonempty
	// selection is a manual override, just like a change between named models.
	if current.Harness != rec.Harness || current.Model != rec.Metadata.Model {
		return nil
	}
	if worker.Reason == "handoff started" && rec.Harness == worker.Target.Harness {
		return s.reason(ctx, rec.ID, "handoff interrupted by daemon restart; inspect the active provider and send the next instruction")
	}
	if rec.HibernatedAt != nil {
		return nil
	}
	failure, turn, err := s.failure(ctx, rec)
	if err != nil || failure.At.IsZero() {
		return err
	}
	if worker.FailureAt.UnixMilli() == failure.At.UnixMilli() &&
		(strings.Contains(worker.Reason, "; left at its failed turn;") || strings.Contains(worker.Reason, "; parked;")) {
		// A manual resume or an unsafe park must not repeat the same action.
		return nil
	}
	failure.Target = current
	if p, ok := domain.AgentAccountProviderFor(rec.Harness); ok && s.accounts != nil {
		account, err := s.accounts.SessionAccountID(ctx, rec.ID, rec.Harness)
		if err == nil && account == domain.DeviceAgentAccountID(p) {
			failure.Scope = "computer login"
		}
	}
	if err := s.store.RecordWorkerQuotaFailure(ctx, rec.ID, failure); err != nil {
		return err
	}
	if domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat {
		return s.park(ctx, rec, turn, failure.At, "automatic Terminal UI handoff unavailable")
	}
	if worker.Attempt != "" {
		// A fallback can also exhaust quota. Never switch this session again.
		return s.park(ctx, rec, turn, failure.At, "automatic handoff budget exhausted")
	}
	eligible := make([]workerquota.Target, 0, len(worker.Remaining))
	for _, t := range worker.Remaining {
		if t.Harness != rec.Harness && s.chat.SupportsChat(t.Harness) {
			eligible = append(eligible, t)
		}
	}
	var defaults map[domain.AgentHarness]string
	if s.policy.DiscoverDefaultModel != nil && len(eligible) > 0 {
		project, found, err := s.store.GetProject(ctx, string(rec.ProjectID))
		if err != nil {
			return err
		}
		if found {
			defaults = s.policy.DefaultModels(ctx, project, eligible)
		}
	}
	now := time.Now()
	failures, err := s.store.WorkerQuotaFailures(ctx, now)
	if err != nil {
		return err
	}
	windows, err := s.policy.Windows(ctx, eligible, failures, now, defaults)
	if err != nil {
		s.log.Debug("worker quota source unavailable", "error", err)
	}
	var target workerquota.Target
	for _, candidate := range eligible {
		if _, ok := workerquota.Select([]workerquota.Target{candidate}, windows, now); !ok {
			continue
		}
		if s.chat.QuotaTargetReady(ctx, rec.ID, candidate.Harness) {
			target = candidate
			break
		}
	}
	if target.Harness == "" {
		return s.park(ctx, rec, turn, failure.At, "no eligible fallback provider")
	}
	if target.Model == "" {
		target.Model = defaults[target.Harness]
	}
	attempt := "worker-quota:" + string(rec.ID)
	claimed, err := s.store.ClaimWorkerQuotaAttempt(ctx, rec.ID, attempt, target)
	if err != nil || !claimed {
		return err
	}
	owner := rec.ControllerOwner()
	request := domain.ProviderSwitchRequest{
		TargetHarness: target.Harness, Model: target.Model, Effort: target.Effort, IdempotencyKey: attempt, ExpectedSource: &owner,
	}
	request.ExpectedQuotaTurnID = turn.ID
	_, switched, err := s.chat.SwitchProviderAndSend(ctx, rec.ID, request, ports.ChatUserMessage{
		ClientMessageID: attempt, Origin: domain.MessageOriginAutomation,
		Text: "Continue the current task using the handoff context and the existing workspace. Inspect the work already done before making changes.",
	})
	if err != nil && switched {
		return s.reason(context.WithoutCancel(ctx), rec.ID, "fallback selected; send a continuation message to resume")
	}
	if err != nil {
		s.log.Debug("quota handoff refused", "session", rec.ID, "target", target.Harness, "error", err)
		return s.park(ctx, rec, turn, failure.At, "fallback handoff refused")
	}
	return s.reason(context.WithoutCancel(ctx), rec.ID, fmt.Sprintf("quota failover selected %s (%s, %s)", target.Harness, target.Model, target.Effort))
}

func (s workerQuotaSweep) failure(ctx context.Context, rec domain.SessionRecord) (workerquota.Failure, domain.ConversationTurn, error) {
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		turn, err := s.chat.QuotaFailure(ctx, rec)
		var f workerquota.Failure
		if turn.CompletedAt != nil {
			f.At = *turn.CompletedAt
		}
		if turn.UsageLimit != nil && turn.UsageLimit.ResetsAt != nil {
			f.ResetAt = *turn.UsageLimit.ResetsAt
		}
		return f, turn, err
	}
	agent, ok := s.agents.Agent(rec.Harness)
	detector, supported := agent.(ports.TranscriptTurnFailureDetector)
	if !ok || !supported || rec.Metadata.NativeTranscriptPath == "" || rec.Metadata.RuntimeLaunchID == "" ||
		rec.Metadata.AgentSessionIDLaunchID != rec.Metadata.RuntimeLaunchID {
		return workerquota.Failure{}, domain.ConversationTurn{}, nil
	}
	failure, failed, err := detector.LastTurnFailure(ctx, rec.Metadata.NativeTranscriptPath)
	if !failed || !failure.QuotaExhausted || failure.Interrupted || failure.At.Before(rec.Activity.LastActivityAt) ||
		failure.At.Before(rec.Metadata.LatestUserPromptAt) {
		return workerquota.Failure{}, domain.ConversationTurn{}, err
	}
	return workerquota.Failure{At: failure.At}, domain.ConversationTurn{}, nil
}

func (s workerQuotaSweep) park(ctx context.Context, rec domain.SessionRecord, turn domain.ConversationTurn, at time.Time, why string) error {
	var parked bool
	var err error
	if turn.ID != "" {
		parked, err = s.chat.ParkQuotaChat(ctx, rec, turn.ID)
	} else if s.terminal != nil {
		parked, err = s.terminal.ParkQuotaTUI(ctx, rec, at)
	}
	action := "left at its failed turn"
	if parked {
		action = "parked"
	}
	if storeErr := s.reason(ctx, rec.ID, why+"; "+action+"; resume this session or switch providers manually"); storeErr != nil {
		return storeErr
	}
	return err
}

func (s workerQuotaSweep) reason(ctx context.Context, id domain.SessionID, reason string) error {
	changed, err := s.store.SetWorkerQuotaReason(ctx, id, reason)
	if changed {
		s.log.Info("worker quota", "session", id, "reason", reason)
	}
	return err
}
