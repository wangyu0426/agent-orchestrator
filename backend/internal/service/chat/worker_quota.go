package chat

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// QuotaFailure returns the current, settled, authoritative provider refusal.
// A failure on a prior native branch or a user-armed recovery belongs to that
// branch's original provider and must not trigger an automatic switch.
func (s *Service) QuotaFailure(ctx context.Context, rec domain.SessionRecord) (domain.ConversationTurn, error) {
	store, ok := s.store.(usageLimitStore)
	if !ok {
		return domain.ConversationTurn{}, nil
	}
	conversation, err := s.store.ConversationForSession(ctx, rec.ID)
	if errors.Is(err, domain.ErrNoConversation) {
		return domain.ConversationTurn{}, nil
	}
	if err != nil {
		return domain.ConversationTurn{}, err
	}
	turn, err := store.LatestAutoResumeTurn(ctx, conversation.ID)
	if errors.Is(err, domain.ErrNoConversationTurn) {
		return domain.ConversationTurn{}, nil
	}
	if err != nil {
		return domain.ConversationTurn{}, err
	}
	if turn.State != domain.TurnStateFailed || turn.FailureCode != domain.ConversationFailureQuotaExhausted ||
		turn.BranchID != conversation.ActiveBranchID || turn.HandledBySessionID != rec.ID ||
		turn.RolledBackAt != nil || turn.CompletedAt == nil || (turn.UsageLimit != nil && turn.UsageLimit.AutoResume) {
		return domain.ConversationTurn{}, nil
	}
	return turn, nil
}

// ParkQuotaChat stops a quiescent provider using the durable hibernation fence.
func (s *Service) ParkQuotaChat(ctx context.Context, rec domain.SessionRecord, turnID string) (bool, error) {
	return s.hibernateChat(ctx, rec.ID, hibernateOptions{quotaTurnID: turnID, quotaOwner: rec.ControllerOwner(), quotaModel: rec.Metadata.Model, ignoreViews: true})
}

// QuotaTargetReady checks that an unattended switch can preserve approvals.
func (s *Service) QuotaTargetReady(ctx context.Context, id domain.SessionID, target domain.AgentHarness) bool {
	source, err := s.Controller(id)
	if err != nil {
		return false
	}
	permissions, _ := s.switchPermissions(ctx, source, domain.ProviderSwitchRequest{TargetHarness: target})
	return permissions == s.sourcePermissions(source) && s.PreflightChat(ctx, target, permissions) == nil
}
