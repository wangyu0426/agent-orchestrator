package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHooks_AgyStopIngestsTranscriptPath(t *testing.T) {
	geminiHome := t.TempDir()
	t.Setenv("GEMINI_HOME", geminiHome)
	t.Setenv("AO_SESSION_ID", "ao-agy-9")
	cfg := setConfigEnv(t)
	srv, capture := activityServer(t, http.StatusOK, `{"ok":true}`)
	writeRunFileFor(t, cfg, srv)

	dir := filepath.Join(geminiHome, "antigravity-cli", "brain", "agy-conv-9", ".system_generated", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	fixture := strings.Join([]string{
		`{"step_index":1,"type":"USER_INPUT","status":"DONE","content":"fix bug 20"}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"Fixed bug 20 and added tests."}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(map[string]any{
		"conversationId": "agy-conv-9",
		"transcriptPath": transcriptPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, _, err := executeCLI(t, Deps{
		In:           strings.NewReader(string(payload)),
		ProcessAlive: func(int) bool { return true },
	}, "hooks", "agy", "stop")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out) != "{}" {
		t.Fatalf("hook output = %q, want {}", out)
	}
	var req setActivityAPIRequest
	if err := json.Unmarshal([]byte(capture.body), &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if req.State != "idle" || req.Event != "stop" {
		t.Fatalf("state/event = %q/%q, want idle/stop", req.State, req.Event)
	}
	if req.TranscriptPath == "" {
		t.Fatalf("expected validated TranscriptPath in activity request")
	}
	if req.TurnUserRequest != "fix bug 20" {
		t.Fatalf("TurnUserRequest = %q, want %q", req.TurnUserRequest, "fix bug 20")
	}
	if req.FinalAssistantMessage != "Fixed bug 20 and added tests." {
		t.Fatalf("FinalAssistantMessage = %q, want %q", req.FinalAssistantMessage, "Fixed bug 20 and added tests.")
	}
	if req.ProviderTurnID != "step:2" {
		t.Fatalf("ProviderTurnID = %q, want %q", req.ProviderTurnID, "step:2")
	}
}
