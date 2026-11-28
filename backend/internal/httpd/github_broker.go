package httpd

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubapp"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubbroker"
)

const maxBrokerRequestBytes = 8 << 10 // 8 KiB

type githubBrokerRequest struct {
	SessionID  string `json:"session_id"`
	Host       string `json:"host,omitempty"`
	Repository string `json:"repository,omitempty"`
}

func mountGitHubBroker(r chi.Router, broker *githubbroker.Broker) {
	r.Post("/internal/github/broker/token", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		if !localControlRequest(req) {
			envelope.WriteJSON(w, http.StatusForbidden, map[string]any{
				"status": "forbidden", "service": daemonmeta.ServiceName,
			})
			return
		}
		if broker == nil {
			envelope.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "github worker credential broker is disabled",
			})
			return
		}
		issueReq, ok := decodeBrokerRequest(w, req)
		if !ok {
			return
		}
		resp, err := broker.IssueToken(req.Context(), issueReq)
		if err != nil {
			writeBrokerError(w, err)
			return
		}
		envelope.WriteJSON(w, http.StatusOK, resp)
	})

	r.Post("/internal/github/broker/invalidate", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		if !localControlRequest(req) {
			envelope.WriteJSON(w, http.StatusForbidden, map[string]any{
				"status": "forbidden", "service": daemonmeta.ServiceName,
			})
			return
		}
		if broker == nil {
			envelope.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "github worker credential broker is disabled",
			})
			return
		}
		issueReq, ok := decodeBrokerRequest(w, req)
		if !ok {
			return
		}
		if err := broker.InvalidateToken(req.Context(), issueReq); err != nil {
			writeBrokerError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func decodeBrokerRequest(w http.ResponseWriter, req *http.Request) (githubbroker.IssueRequest, bool) {
	token, ok := parseBrokerBearerToken(req.Header.Get("Authorization"))
	if !ok {
		envelope.WriteJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "missing or invalid bearer token",
		})
		return githubbroker.IssueRequest{}, false
	}

	var body githubBrokerRequest
	dec := json.NewDecoder(io.LimitReader(req.Body, maxBrokerRequestBytes))
	if err := dec.Decode(&body); err != nil {
		envelope.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid json request body",
		})
		return githubbroker.IssueRequest{}, false
	}

	return githubbroker.IssueRequest{
		SessionID:   domain.SessionID(strings.TrimSpace(body.SessionID)),
		BrokerToken: token,
		Host:        strings.TrimSpace(body.Host),
		Repository:  strings.TrimSpace(body.Repository),
	}, true
}

func parseBrokerBearerToken(header string) (string, bool) {
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	tok := strings.TrimSpace(parts[1])
	if tok == "" {
		return "", false
	}
	return tok, true
}

func writeBrokerError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, githubbroker.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, githubbroker.ErrForbiddenRepo):
		status = http.StatusForbidden
	case errors.Is(err, githubbroker.ErrInvalidRequest):
		status = http.StatusBadRequest
	case errors.Is(err, githubbroker.ErrDisabled):
		status = http.StatusServiceUnavailable
	}
	envelope.WriteJSON(w, status, map[string]any{
		"error": githubapp.Redact(err.Error()),
	})
}
