package chat

import (
	"context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// PrepareChatMergeArchive checks the same provider and durable turn facts as
// idle hibernation. Apply additionally fences intake until the caller has
// finished teardown and persisted termination. Preview never stops or fences
// the controller beyond the read's dispatch lock.
func (s *Service) PrepareChatMergeArchive(ctx context.Context, expected domain.SessionRecord, hold bool) (func(context.Context) error, func(), error) {
	id := expected.ID
	gate := s.controllerGate(domain.SessionConversationOwner(id))
	if !gate.tryLock() {
		return nil, nil, ErrTurnRunning
	}
	keepGate := false
	defer func() {
		if !keepGate {
			gate.unlock()
		}
	}()

	matches := func(rec domain.SessionRecord) bool {
		return rec.Kind == domain.KindWorker && rec.EligibleForChatHibernation() &&
			rec.ControllerOwner() == expected.ControllerOwner() && rec.Revision == expected.Revision &&
			!s.isWaking(id) && !s.accountMoveAwaitingTarget(ctx, id)
	}
	rec, err := s.requireChatSession(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if !matches(rec) {
		return nil, nil, ErrTurnRunning
	}
	controller, err := s.Controller(id)
	if err != nil {
		return nil, nil, err
	}
	if controller.State() != ports.ChatControllerReady || controller.generation != rec.Metadata.ControllerGeneration {
		return nil, nil, ErrTurnRunning
	}
	hibernator, ok := controller.conv.(ports.ChatProviderHibernator)
	if !ok {
		return nil, nil, errors.New("chat provider cannot confirm idle background work")
	}
	// Provider inventory may need its event stream. Do not hold sendMu while
	// asking it; recheck all dispatch and durable facts after the probe.
	ready, err := hibernator.CanHibernate(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("check chat background work: %w", err)
	}
	if !ready || !controller.sendMu.TryLock() {
		return nil, nil, ErrTurnRunning
	}
	defer controller.sendMu.Unlock()
	controller.mu.Lock()
	busy := controller.state != ports.ChatControllerReady ||
		controller.handoff != controllerHandoffNone ||
		controller.pendingTurnID != "" || controller.dispatchingTurnID != "" ||
		controller.compactionPending || controller.operations != 0 ||
		controller.forkDone != nil
	controller.mu.Unlock()
	if busy {
		return nil, nil, ErrTurnRunning
	}

	if _, err := s.store.NextQueuedTurn(ctx, controller.conversation.ID); err == nil {
		return nil, nil, ErrTurnRunning
	} else if !errors.Is(err, domain.ErrNoQueuedTurn) {
		return nil, nil, err
	}
	if running, err := s.store.ListVisibleRunningTurnProviderIDs(ctx, controller.conversation.ID); err != nil {
		return nil, nil, err
	} else if len(running) != 0 {
		return nil, nil, ErrTurnRunning
	}
	if pending, err := s.store.HasPendingConversationInteractions(ctx, controller.conversation.ID); err != nil {
		return nil, nil, err
	} else if pending {
		return nil, nil, ErrTurnRunning
	}
	turns, ok := s.sessions.(hibernationTurnReader)
	if !ok {
		return nil, nil, errors.New("chat turn state is unavailable")
	}
	settled, err := turns.LatestVisibleUserTurnSettled(ctx, controller.conversation.ID, id)
	if err != nil {
		return nil, nil, err
	}
	if !settled {
		return nil, nil, ErrTurnRunning
	}
	if s.sides != nil {
		sides, err := s.sides.store.ListSideConversations(ctx, id, s.sides.launchID())
		if err != nil {
			return nil, nil, err
		}
		if len(sides) != 0 {
			return nil, nil, errors.New("an independent side chat is still open")
		}
	}
	fresh, err := s.requireChatSession(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if !matches(fresh) {
		return nil, nil, ErrTurnRunning
	}
	if !hold {
		return nil, nil, nil
	}

	// Reuse the branch-handoff fence: even a sender holding an earlier
	// controller pointer cannot enqueue work once sendMu is released.
	controller.mu.Lock()
	controller.handoff = controllerHandoffIdleBranch
	controller.branchHandoffDone = make(chan struct{})
	controller.mu.Unlock()
	keepGate = true
	stopping := false
	stop := func(ctx context.Context) error {
		stopping = true
		return s.stopLocked(ctx, id)
	}
	release := func() {
		if stopping {
			// A stop failure may leave a dying provider. Never reopen intake.
			controller.completeBranchHandoff()
		} else {
			controller.AbortHandoff()
		}
		gate.unlock()
	}
	return stop, release, nil
}
