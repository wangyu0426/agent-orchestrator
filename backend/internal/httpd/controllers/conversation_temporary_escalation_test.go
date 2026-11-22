package controllers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	escalationsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/escalation"
)

type fakeTemporaryEscalationService struct {
	esc       *domain.TemporaryEscalation
	found     bool
	startErr  error
	getErr    error
	actionErr error
	startReq  domain.ProviderSwitchRequest
	started   bool
	cancelled bool
	restored  bool
	retried   bool
	boundTurn string
	bindErr   error
}

func (f *fakeTemporaryEscalationService) Start(_ context.Context, _ domain.SessionID, req domain.ProviderSwitchRequest) (*domain.TemporaryEscalation, error) {
	f.started = true
	f.startReq = req
	if f.startErr != nil {
		return nil, f.startErr
	}
	return f.esc, nil
}

func (f *fakeTemporaryEscalationService) BindWorkItem(_ context.Context, _ domain.SessionID, turnID string) error {
	f.boundTurn = turnID
	return f.bindErr
}

func (f *fakeTemporaryEscalationService) Get(_ context.Context, _ domain.SessionID) (*domain.TemporaryEscalation, bool, error) {
	if f.getErr != nil {
		return nil, false, f.getErr
	}
	return f.esc, f.found, nil
}

func (f *fakeTemporaryEscalationService) Cancel(_ context.Context, _ domain.SessionID) error {
	f.cancelled = true
	return f.actionErr
}

func (f *fakeTemporaryEscalationService) RestoreNow(_ context.Context, _ domain.SessionID) error {
	f.restored = true
	return f.actionErr
}

func (f *fakeTemporaryEscalationService) RetryRevert(_ context.Context, _ domain.SessionID) error {
	f.retried = true
	return f.actionErr
}

func testEscalation(t *testing.T) *domain.TemporaryEscalation {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	return &domain.TemporaryEscalation{
		ID:        "esc-1",
		SessionID: "s1",
		State:     domain.TemporaryEscalationPending,
		Original: domain.ModelSnapshot{
			Harness: domain.HarnessCodex, Model: "gpt-5", Effort: "medium",
		},
		TargetHarness: domain.HarnessClaudeCode, TargetModel: "claude-opus-4-6", TargetEffort: "high",
		CreatedAt: now, UpdatedAt: now,
	}
}

func serveWithEscalation(svc controllers.ConversationService, esc controllers.TemporaryEscalationService, req *http.Request) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	(&controllers.ConversationsController{Svc: svc, Escalation: esc}).Register(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestTemporaryEscalationGetReportsNoneWhenAbsent(t *testing.T) {
	esc := &fakeTemporaryEscalationService{found: false}
	rec := serveWithEscalation(&fakeConversationService{}, esc,
		httptest.NewRequest(http.MethodGet, "/sessions/s1/temporary-escalation", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "none" {
		t.Fatalf("body = %v, want status none", body)
	}
}

func TestTemporaryEscalationGetReturnsState(t *testing.T) {
	esc := &fakeTemporaryEscalationService{esc: testEscalation(t), found: true}
	rec := serveWithEscalation(&fakeConversationService{}, esc,
		httptest.NewRequest(http.MethodGet, "/sessions/s1/temporary-escalation", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "pending" || body["originalModel"] != "gpt-5" || body["targetModel"] != "claude-opus-4-6" {
		t.Fatalf("body = %v", body)
	}
}

func TestTemporaryEscalationGetWithoutServiceIsNotImplemented(t *testing.T) {
	rec := serveWithEscalation(&fakeConversationService{}, nil,
		httptest.NewRequest(http.MethodGet, "/sessions/s1/temporary-escalation", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("get = %d %s, want 501", rec.Code, rec.Body.String())
	}
}

func TestTemporaryProviderSwitchRoutesToEscalationService(t *testing.T) {
	svc := &fakeConversationService{}
	esc := &fakeTemporaryEscalationService{esc: testEscalation(t), found: true}
	rec := serveWithEscalation(svc, esc, httptest.NewRequest(http.MethodPost,
		"/sessions/s1/provider-switch", bytes.NewBufferString(`{"targetHarness":"claude-code","model":"claude-opus-4-6","effort":"high","temporary":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("switch = %d %s", rec.Code, rec.Body.String())
	}
	if !esc.started || esc.startReq.TargetHarness != domain.HarnessClaudeCode || esc.startReq.Model != "claude-opus-4-6" {
		t.Fatalf("escalation start = %+v started=%v", esc.startReq, esc.started)
	}
	if svc.switchRequest.TargetHarness != "" {
		t.Fatalf("permanent switch also ran: %+v", svc.switchRequest)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	temp, ok := body["temporaryEscalation"].(map[string]any)
	if !ok || temp["status"] != "pending" {
		t.Fatalf("body = %v", body)
	}
}

func TestTemporaryProviderSwitchWithMessageSendsAfterStart(t *testing.T) {
	svc := &fakeConversationService{}
	esc := &fakeTemporaryEscalationService{esc: testEscalation(t), found: true}
	rec := serveWithEscalation(svc, esc, httptest.NewRequest(http.MethodPost,
		"/sessions/s1/provider-switch", bytes.NewBufferString(`{"targetHarness":"claude-code","temporary":true,"message":{"text":"continue"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("switch = %d %s", rec.Code, rec.Body.String())
	}
	if !esc.started || svc.sent.Text != "continue" {
		t.Fatalf("started=%v sent=%+v", esc.started, svc.sent)
	}
	// The new turn created by the send is the escalated work item.
	if esc.boundTurn != "turn-1" {
		t.Fatalf("boundTurn = %q, want turn-1", esc.boundTurn)
	}
}

func TestTemporaryProviderSwitchMapsEscalationErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{escalationsvc.ErrEscalationBusy, http.StatusConflict, "TEMPORARY_ESCALATION_BUSY"},
		{escalationsvc.ErrEscalationUnsupported, http.StatusBadRequest, "TEMPORARY_ESCALATION_UNSUPPORTED"},
		{escalationsvc.ErrSessionNotFound, http.StatusNotFound, "TEMPORARY_ESCALATION_NOT_FOUND"},
	} {
		svc := &fakeConversationService{}
		esc := &fakeTemporaryEscalationService{startErr: tc.err}
		rec := serveWithEscalation(svc, esc, httptest.NewRequest(http.MethodPost,
			"/sessions/s1/provider-switch", bytes.NewBufferString(`{"targetHarness":"claude-code","temporary":true}`)))
		if rec.Code != tc.status || !bytes.Contains(rec.Body.Bytes(), []byte(tc.code)) {
			t.Fatalf("err %v: status=%d body=%s, want %d %s", tc.err, rec.Code, rec.Body.String(), tc.status, tc.code)
		}
	}
}

func TestTemporaryEscalationActions(t *testing.T) {
	for _, tc := range []struct {
		path string
		flag func(*fakeTemporaryEscalationService) bool
	}{
		{"/sessions/s1/temporary-escalation/cancel", func(f *fakeTemporaryEscalationService) bool { return f.cancelled }},
		{"/sessions/s1/temporary-escalation/restore", func(f *fakeTemporaryEscalationService) bool { return f.restored }},
		{"/sessions/s1/temporary-escalation/retry", func(f *fakeTemporaryEscalationService) bool { return f.retried }},
	} {
		esc := &fakeTemporaryEscalationService{esc: testEscalation(t), found: true}
		rec := serveWithEscalation(&fakeConversationService{}, esc,
			httptest.NewRequest(http.MethodPost, tc.path, nil))
		if rec.Code != http.StatusOK || !tc.flag(esc) {
			t.Fatalf("%s: status=%d body=%s flag=%v", tc.path, rec.Code, rec.Body.String(), tc.flag(esc))
		}
	}
}

func TestTemporaryEscalationRestoreWithoutEscalationIsNotFound(t *testing.T) {
	esc := &fakeTemporaryEscalationService{actionErr: escalationsvc.ErrNoEscalation}
	rec := serveWithEscalation(&fakeConversationService{}, esc,
		httptest.NewRequest(http.MethodPost, "/sessions/s1/temporary-escalation/restore", nil))
	if rec.Code != http.StatusNotFound || !bytes.Contains(rec.Body.Bytes(), []byte("TEMPORARY_ESCALATION_NOT_FOUND")) {
		t.Fatalf("restore = %d %s, want 404 TEMPORARY_ESCALATION_NOT_FOUND", rec.Code, rec.Body.String())
	}
}
