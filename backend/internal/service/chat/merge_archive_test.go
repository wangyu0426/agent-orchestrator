package chat_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestChatMergeArchiveRefusesNewWorkDuringProbe(t *testing.T) {
	h, conv := settledHibernationHarness(t, domain.TurnStateCompleted)
	ctx := t.Context()
	rec, found, err := h.st.GetSession(ctx, testSession)
	if err != nil || !found {
		t.Fatalf("read session: %v, %v", found, err)
	}
	var sent bool
	conv.onBackgroundCheck = func(ctx context.Context) {
		// Model a sender that obtained the source controller before archive
		// acquired its gate but did not dispatch until the provider probe.
		if _, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "new task"}); err != nil {
			t.Fatal(err)
		}
		sent = true
	}
	stop, release, err := h.svc.PrepareChatMergeArchive(ctx, rec, true)
	if release != nil {
		release()
	}
	if !sent || err == nil || stop != nil || !h.svc.HasLiveChatController(testSession) {
		t.Fatalf("archive admitted concurrent work: sent=%v err=%v", sent, err)
	}
}

func TestChatMergeArchiveFencesInputUntilTeardown(t *testing.T) {
	h, _ := settledHibernationHarness(t, domain.TurnStateCompleted)
	ctx := t.Context()
	rec, found, err := h.st.GetSession(ctx, testSession)
	if err != nil || !found {
		t.Fatalf("read session: %v, %v", found, err)
	}
	if _, _, err := h.svc.PrepareChatMergeArchive(ctx, rec, false); err != nil {
		t.Fatalf("preview: %v", err)
	}
	stop, release, err := h.svc.PrepareChatMergeArchive(ctx, rec, true)
	if err != nil || stop == nil || release == nil {
		t.Fatalf("archive fence: %v", err)
	}
	defer release()
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "new task"}); !errors.Is(err, chatsvc.ErrControllerHandoff) {
		t.Fatalf("service send crossed archive fence: %v", err)
	}
	if _, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "captured controller"}); !errors.Is(err, chatsvc.ErrControllerHandoff) {
		t.Fatalf("captured controller crossed archive fence: %v", err)
	}
	if err := stop(ctx); err != nil {
		t.Fatalf("stop fenced controller: %v", err)
	}
	if h.svc.HasLiveChatController(testSession) {
		t.Fatal("archive stop left the controller live")
	}
}
