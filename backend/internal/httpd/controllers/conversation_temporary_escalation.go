package controllers

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	escalationsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/escalation"
)

func newTemporaryEscalationResponse(esc *domain.TemporaryEscalation) *TemporaryEscalationResponse {
	if esc == nil {
		return &TemporaryEscalationResponse{Status: "none"}
	}
	return &TemporaryEscalationResponse{
		Status:          string(esc.State),
		SessionID:       string(esc.SessionID),
		OriginalHarness: string(esc.Original.Harness),
		OriginalModel:   esc.Original.Model,
		OriginalEffort:  esc.Original.Effort,
		TargetHarness:   string(esc.TargetHarness),
		TargetModel:     esc.TargetModel,
		TargetEffort:    esc.TargetEffort,
		State:           string(esc.State),
		DoneReportID:    esc.DoneReportID,
		LastError:       esc.LastError,
		CreatedAt:       esc.CreatedAt,
		UpdatedAt:       esc.UpdatedAt,
		RevertedAt:      esc.RevertedAt,
	}
}

// writeTemporaryEscalationError maps escalation failures onto the API
// error envelope. A failed revert keeps the worker on the upgraded model;
// the error names that, and the retry/manual-restore routes stay open.
func writeTemporaryEscalationError(w http.ResponseWriter, r *http.Request, err error) {
	status, category, code := http.StatusInternalServerError, "internal", "TEMPORARY_ESCALATION_FAILED"
	switch {
	case errors.Is(err, escalationsvc.ErrEscalationUnsupported):
		status, category, code = http.StatusBadRequest, "validation", "TEMPORARY_ESCALATION_UNSUPPORTED"
	case errors.Is(err, escalationsvc.ErrSessionNotFound), errors.Is(err, escalationsvc.ErrNoEscalation):
		status, category, code = http.StatusNotFound, "not_found", "TEMPORARY_ESCALATION_NOT_FOUND"
	case errors.Is(err, escalationsvc.ErrEscalationActive):
		status, category, code = http.StatusConflict, "conflict", "TEMPORARY_ESCALATION_ACTIVE"
	case errors.Is(err, escalationsvc.ErrEscalationBusy), errors.Is(err, escalationsvc.ErrPendingInteraction):
		status, category, code = http.StatusConflict, "conflict", "TEMPORARY_ESCALATION_BUSY"
	case errors.Is(err, escalationsvc.ErrEscalationNotIdle):
		status, category, code = http.StatusConflict, "conflict", "TEMPORARY_ESCALATION_NOT_IDLE"
	}
	envelope.WriteAPIError(w, r, status, category, code, err.Error(), nil)
}

func (c *ConversationsController) escalationService(w http.ResponseWriter, r *http.Request, specPath string) (TemporaryEscalationService, bool) {
	if c.Escalation == nil {
		apispec.NotImplemented(w, r, r.Method, specPath)
		return nil, false
	}
	return c.Escalation, true
}

func (c *ConversationsController) temporaryEscalation(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.escalationService(w, r, "/api/v1/sessions/{sessionId}/temporary-escalation")
	if !ok {
		return
	}
	esc, found, err := svc.Get(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")))
	if err != nil {
		writeConversationError(w, r, err)
		return
	}
	if !found {
		envelope.WriteJSON(w, http.StatusOK, TemporaryEscalationResponse{Status: "none"})
		return
	}
	envelope.WriteJSON(w, http.StatusOK, newTemporaryEscalationResponse(esc))
}

func (c *ConversationsController) temporaryEscalationAction(
	w http.ResponseWriter, r *http.Request, specPath string,
	action func(ctx context.Context, id domain.SessionID) error,
) {
	svc, ok := c.escalationService(w, r, specPath)
	if !ok {
		return
	}
	id := domain.SessionID(chi.URLParam(r, "sessionId"))
	if err := action(r.Context(), id); err != nil {
		writeTemporaryEscalationError(w, r, err)
		return
	}
	esc, found, err := svc.Get(r.Context(), id)
	if err != nil {
		writeConversationError(w, r, err)
		return
	}
	if !found {
		envelope.WriteJSON(w, http.StatusOK, TemporaryEscalationResponse{Status: "none"})
		return
	}
	envelope.WriteJSON(w, http.StatusOK, newTemporaryEscalationResponse(esc))
}

func (c *ConversationsController) cancelTemporaryEscalation(w http.ResponseWriter, r *http.Request) {
	c.temporaryEscalationAction(w, r, "/api/v1/sessions/{sessionId}/temporary-escalation/cancel", func(ctx context.Context, id domain.SessionID) error {
		return c.Escalation.Cancel(ctx, id)
	})
}

func (c *ConversationsController) restoreTemporaryEscalation(w http.ResponseWriter, r *http.Request) {
	c.temporaryEscalationAction(w, r, "/api/v1/sessions/{sessionId}/temporary-escalation/restore", func(ctx context.Context, id domain.SessionID) error {
		return c.Escalation.RestoreNow(ctx, id)
	})
}

func (c *ConversationsController) retryTemporaryEscalation(w http.ResponseWriter, r *http.Request) {
	c.temporaryEscalationAction(w, r, "/api/v1/sessions/{sessionId}/temporary-escalation/retry", func(ctx context.Context, id domain.SessionID) error {
		return c.Escalation.RetryRevert(ctx, id)
	})
}
