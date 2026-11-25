package agy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupAgyTranscriptFixture(t *testing.T, convID string, lines []string) (geminiHome, transcriptPath string) {
	t.Helper()
	geminiHome = t.TempDir()
	t.Setenv("GEMINI_HOME", geminiHome)
	dir := filepath.Join(geminiHome, CLIStorageDir, "brain", convID, ".system_generated", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	transcriptPath = filepath.Join(dir, "transcript.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return geminiHome, transcriptPath
}

func fastTranscriptTimers(t *testing.T) {
	t.Helper()
	origWait, origPoll := agyTranscriptWait, agyTranscriptPoll
	agyTranscriptWait = 50 * time.Millisecond
	agyTranscriptPoll = 5 * time.Millisecond
	t.Cleanup(func() {
		agyTranscriptWait = origWait
		agyTranscriptPoll = origPoll
	})
}

func TestResolveStopTranscript_SingleAndMultipleTurns(t *testing.T) {
	fastTranscriptTimers(t)

	_, path := setupAgyTranscriptFixture(t, "conv-1", []string{
		`{"step_index":1,"type":"USER_INPUT","status":"DONE","content":"first request"}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"read_file"}]}`,
		`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","content":"first answer"}`,
		`{"step_index":4,"type":"USER_INPUT","status":"DONE","content":"second request"}`,
		`{"step_index":5,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"edit_file"}]}`,
		`{"step_index":6,"type":"PLANNER_RESPONSE","status":"DONE","content":"second final answer"}`,
	})

	payload, err := json.Marshal(map[string]any{
		"conversationId": "conv-1",
		"transcriptPath": path,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := ResolveStopTranscript(payload)
	if got.TranscriptPath == "" {
		t.Fatalf("expected validated TranscriptPath, got empty")
	}
	if got.TurnUserRequest != "second request" {
		t.Fatalf("TurnUserRequest = %q, want %q", got.TurnUserRequest, "second request")
	}
	if got.FinalAssistantMessage != "second final answer" {
		t.Fatalf("FinalAssistantMessage = %q, want %q", got.FinalAssistantMessage, "second final answer")
	}
	if got.ProviderTurnID != "step:6" {
		t.Fatalf("ProviderTurnID = %q, want %q", got.ProviderTurnID, "step:6")
	}
}

func TestResolveStopTranscript_IncompleteOrFailedTurnNeverFallsBack(t *testing.T) {
	fastTranscriptTimers(t)

	cases := []struct {
		name  string
		lines []string
	}{
		{
			name: "running planner response",
			lines: []string{
				`{"step_index":1,"type":"USER_INPUT","status":"DONE","content":"turn 1"}`,
				`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"turn 1 answer"}`,
				`{"step_index":3,"type":"USER_INPUT","status":"DONE","content":"turn 2"}`,
				`{"step_index":4,"type":"PLANNER_RESPONSE","status":"RUNNING","content":"partial"}`,
			},
		},
		{
			name: "errored planner response",
			lines: []string{
				`{"step_index":1,"type":"USER_INPUT","status":"DONE","content":"turn 1"}`,
				`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"turn 1 answer"}`,
				`{"step_index":3,"type":"USER_INPUT","status":"DONE","content":"turn 2"}`,
				`{"step_index":4,"type":"PLANNER_RESPONSE","status":"ERROR","content":"failed"}`,
			},
		},
		{
			name: "user input without response",
			lines: []string{
				`{"step_index":1,"type":"USER_INPUT","status":"DONE","content":"turn 1"}`,
				`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"turn 1 answer"}`,
				`{"step_index":3,"type":"USER_INPUT","status":"DONE","content":"turn 2"}`,
			},
		},
		{
			name: "tool calls only without final text",
			lines: []string{
				`{"step_index":1,"type":"USER_INPUT","status":"DONE","content":"turn 1"}`,
				`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"turn 1 answer"}`,
				`{"step_index":3,"type":"USER_INPUT","status":"DONE","content":"turn 2"}`,
				`{"step_index":4,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"run_command"}]}`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, path := setupAgyTranscriptFixture(t, "conv-incomplete", tc.lines)
			payload, _ := json.Marshal(map[string]any{
				"conversationId": "conv-incomplete",
				"transcriptPath": path,
			})
			got := ResolveStopTranscript(payload)
			if got.FinalAssistantMessage != "" {
				t.Fatalf("FinalAssistantMessage = %q, want empty (must not fall back to earlier turn)", got.FinalAssistantMessage)
			}
		})
	}
}

func TestResolveStopTranscript_WaitsForLateFlush(t *testing.T) {
	origWait, origPoll := agyTranscriptWait, agyTranscriptPoll
	agyTranscriptWait = 300 * time.Millisecond
	agyTranscriptPoll = 10 * time.Millisecond
	t.Cleanup(func() {
		agyTranscriptWait = origWait
		agyTranscriptPoll = origPoll
	})

	_, path := setupAgyTranscriptFixture(t, "conv-race", []string{
		`{"step_index":1,"type":"USER_INPUT","status":"DONE","content":"ship it"}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","status":"RUNNING"}`,
	})
	go func() {
		time.Sleep(25 * time.Millisecond)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		_, _ = f.WriteString(`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","content":"shipped"}` + "\n")
		_ = f.Close()
	}()

	payload, _ := json.Marshal(map[string]any{
		"conversationId": "conv-race",
		"transcriptPath": path,
	})
	got := ResolveStopTranscript(payload)
	if got.FinalAssistantMessage != "shipped" {
		t.Fatalf("FinalAssistantMessage = %q, want %q", got.FinalAssistantMessage, "shipped")
	}
}

func TestResolveStopTranscript_MalformedAndOversized(t *testing.T) {
	fastTranscriptTimers(t)

	t.Run("malformed lines skipped", func(t *testing.T) {
		_, path := setupAgyTranscriptFixture(t, "conv-malformed", []string{
			`not-json`,
			`{"step_index":1,"type":"USER_INPUT","status":"DONE","content":"check"}`,
			`{truncated`,
			`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"all good"}`,
		})
		payload, _ := json.Marshal(map[string]any{"conversationId": "conv-malformed", "transcriptPath": path})
		got := ResolveStopTranscript(payload)
		if got.FinalAssistantMessage != "all good" || got.TurnUserRequest != "check" {
			t.Fatalf("got %+v, want request=check response=all good", got)
		}
	})

	t.Run("malformed tail boundary fails closed", func(t *testing.T) {
		_, path := setupAgyTranscriptFixture(t, "conv-tail-bad", []string{
			`{"step_index":1,"type":"USER_INPUT","status":"DONE","content":"turn 1"}`,
			`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"turn 1 answer"}`,
			`{"step_index":3,"type":"USER_INPUT","status":"DONE","content":"turn 2 partial`,
		})
		payload, _ := json.Marshal(map[string]any{"conversationId": "conv-tail-bad", "transcriptPath": path})
		got := ResolveStopTranscript(payload)
		if got.FinalAssistantMessage != "" {
			t.Fatalf("expected empty FinalAssistantMessage on malformed tail boundary, got %+v", got)
		}
	})

	t.Run("oversized file bounded tail read", func(t *testing.T) {
		geminiHome := t.TempDir()
		t.Setenv("GEMINI_HOME", geminiHome)
		dir := filepath.Join(geminiHome, CLIStorageDir, "brain", "conv-big", ".system_generated", "logs")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "transcript.jsonl")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		pad := strings.Repeat("x", 1024)
		for range 5000 {
			_, _ = f.WriteString(`{"step_index":1,"type":"GENERIC","status":"DONE","content":"` + pad + `"}` + "\n")
		}
		_, _ = f.WriteString(`{"step_index":5001,"type":"USER_INPUT","status":"DONE","content":"final q"}` + "\n")
		_, _ = f.WriteString(`{"step_index":5002,"type":"PLANNER_RESPONSE","status":"DONE","content":"final a"}` + "\n")
		_ = f.Close()

		payload, _ := json.Marshal(map[string]any{"conversationId": "conv-big", "transcriptPath": path})
		got := ResolveStopTranscript(payload)
		if got.FinalAssistantMessage != "final a" || got.TurnUserRequest != "final q" {
			t.Fatalf("oversized tail read got %+v", got)
		}
	})
}

func TestResolveStopTranscript_RejectsUnsafePathsAndCrossSession(t *testing.T) {
	origWait, origPoll := agyTranscriptWait, agyTranscriptPoll
	agyTranscriptWait = 2 * time.Second
	agyTranscriptPoll = 20 * time.Millisecond
	t.Cleanup(func() {
		agyTranscriptWait = origWait
		agyTranscriptPoll = origPoll
	})

	geminiHome, validPath := setupAgyTranscriptFixture(t, "conv-owner", []string{
		`{"step_index":1,"type":"USER_INPUT","status":"DONE","content":"secret"}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","content":"secret answer"}`,
	})

	outsideFile := filepath.Join(t.TempDir(), "transcript.jsonl")
	_ = os.WriteFile(outsideFile, []byte(`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","content":"outside"}`+"\n"), 0o600)

	symlinkEscape := filepath.Join(geminiHome, CLIStorageDir, "brain", "conv-owner", "escape.jsonl")
	if err := os.Symlink(outsideFile, symlinkEscape); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	nonJSONL := filepath.Join(geminiHome, CLIStorageDir, "brain", "conv-owner", "notes.txt")
	_ = os.WriteFile(nonJSONL, []byte(`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","content":"txt"}`+"\n"), 0o600)

	unrecognizedLayout := filepath.Join(geminiHome, CLIStorageDir, "other.jsonl")
	_ = os.WriteFile(unrecognizedLayout, []byte(`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","content":"other"}`+"\n"), 0o600)

	// brain/conv-alias itself is a symlink to brain/conv-owner.
	aliasBrainDir := filepath.Join(geminiHome, CLIStorageDir, "brain", "conv-alias")
	ownerBrainDir := filepath.Join(geminiHome, CLIStorageDir, "brain", "conv-owner")
	if err := os.Symlink(ownerBrainDir, aliasBrainDir); err != nil {
		t.Fatalf("symlink brain dir: %v", err)
	}
	aliasTranscript := filepath.Join(aliasBrainDir, ".system_generated", "logs", "transcript.jsonl")

	cases := []struct {
		name           string
		conversationID string
		transcriptPath string
	}{
		{"relative path", "conv-owner", "transcript.jsonl"},
		{"outside trusted root", "conv-owner", outsideFile},
		{"symlink escape", "conv-owner", symlinkEscape},
		{"non-jsonl extension", "conv-owner", nonJSONL},
		{"cross-session mismatch", "conv-other", validPath},
		{"missing conversation id", "", validPath},
		{"unrecognized layout without brain dir", "conv-owner", unrecognizedLayout},
		{"symlinked brain conversation dir", "conv-alias", aliasTranscript},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := json.Marshal(map[string]any{
				"conversationId": tc.conversationID,
				"transcriptPath": tc.transcriptPath,
			})
			start := time.Now()
			got := ResolveStopTranscript(payload)
			if got.TranscriptPath != "" || got.FinalAssistantMessage != "" {
				t.Fatalf("expected rejection for %s, got %+v", tc.name, got)
			}
			if time.Since(start) > 200*time.Millisecond {
				t.Fatalf("unsafe path %s should fail fast without waiting", tc.name)
			}
		})
	}

	t.Run("conflicting session_id and conversation_id aliases rejected", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]any{
			"session_id":      "conv-other",
			"conversation_id": "conv-owner",
			"transcriptPath":  validPath,
		})
		got := ResolveStopTranscript(payload)
		if got.TranscriptPath != "" || got.FinalAssistantMessage != "" {
			t.Fatalf("expected rejection for conflicting aliases, got %+v", got)
		}
	})
}
