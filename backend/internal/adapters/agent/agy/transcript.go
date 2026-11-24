package agy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// agyTranscriptTailBytes bounds how much of a transcript file is read from
	// the end so oversized transcripts cannot exhaust memory or stall hooks.
	agyTranscriptTailBytes = 4 << 20
)

// AGY may still be flushing the final PLANNER_RESPONSE line when Stop fires.
// Keep the wait short so activity hooks never stall; variables allow tests to
// override timing without sleeping.
var (
	agyTranscriptWait = 300 * time.Millisecond
	agyTranscriptPoll = 30 * time.Millisecond
)

// CompletedTurn holds the validated native transcript reference and the final
// completed turn extracted from an AGY Stop hook payload.
type CompletedTurn struct {
	TranscriptPath        string
	TurnUserRequest       string
	FinalAssistantMessage string
	ProviderTurnID        string
}

type agyStopPayload struct {
	TranscriptPath      string `json:"transcript_path"`
	TranscriptPathCamel string `json:"transcriptPath"`
	ConversationID      string `json:"conversation_id"`
	ConversationIDCamel string `json:"conversationId"`
	SessionID           string `json:"session_id"`
	SessionIDCamel      string `json:"sessionId"`
}

type agyTranscriptStep struct {
	StepIndex int               `json:"step_index"`
	Type      string            `json:"type"`
	Status    string            `json:"status"`
	Content   string            `json:"content"`
	ToolCalls []json.RawMessage `json:"tool_calls"`
}

// ValidateTranscriptPath validates that rawPath is an absolute, regular .jsonl
// file inside a trusted AGY storage root under GEMINI_HOME (without symlink
// traversal outside that root). When conversationID is non-empty and the path
// lies under a brain/<id>/ directory, the directory id must match.
func ValidateTranscriptPath(rawPath, conversationID string) (string, bool) {
	path := strings.TrimSpace(rawPath)
	if path == "" || !filepath.IsAbs(path) {
		return "", false
	}
	if !strings.EqualFold(filepath.Ext(path), ".jsonl") {
		return "", false
	}
	geminiHome, err := GeminiHome(nil)
	if err != nil || strings.TrimSpace(geminiHome) == "" {
		return "", false
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(resolvedPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	if !isWithinTrustedAgyRoot(resolvedPath, geminiHome) {
		return "", false
	}
	if convID := strings.TrimSpace(conversationID); convID != "" {
		if pathConvID, ok := brainConversationID(resolvedPath); ok && pathConvID != convID {
			return "", false
		}
	}
	return resolvedPath, true
}

func isWithinTrustedAgyRoot(resolvedPath, geminiHome string) bool {
	trustedDirs := []string{
		filepath.Join(geminiHome, CLIStorageDir),
		filepath.Join(geminiHome, ACPStorageDir),
		filepath.Join(geminiHome, "antigravity"),
	}
	for _, root := range trustedDirs {
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(resolvedRoot, resolvedPath)
		if err != nil {
			continue
		}
		if rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

// brainConversationID extracts <id> from .../brain/<id>/... when present.
func brainConversationID(path string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for i := 0; i < len(parts)-2; i++ {
		if parts[i] == "brain" && parts[i+1] != "" {
			return parts[i+1], true
		}
	}
	return "", false
}

// ResolveStopTranscript validates the transcriptPath in an AGY Stop hook
// payload and extracts the completed turn's final assistant response and
// matching user request. It never returns an earlier turn's response when the
// latest turn is incomplete or failed, and never reads outside trusted AGY
// data roots.
func ResolveStopTranscript(payload []byte) CompletedTurn {
	var in agyStopPayload
	if err := json.Unmarshal(payload, &in); err != nil {
		return CompletedTurn{}
	}
	rawPath := firstNonEmpty(in.TranscriptPath, in.TranscriptPathCamel)
	convID := firstNonEmpty(in.ConversationID, in.ConversationIDCamel, in.SessionID, in.SessionIDCamel)
	if rawPath == "" {
		return CompletedTurn{}
	}

	deadline := time.Now().Add(agyTranscriptWait)
	var lastSize int64 = -1
	var validatedPath string

	for {
		path, ok := ValidateTranscriptPath(rawPath, convID)
		if ok {
			validatedPath = path
			if info, err := os.Stat(path); err == nil && info.Size() != lastSize {
				lastSize = info.Size()
				turn, settled := parseAgyTranscriptTail(path)
				turn.TranscriptPath = path
				if settled || turn.FinalAssistantMessage != "" {
					return turn
				}
			}
		} else if !isTransientMissing(rawPath) {
			// Unsafe path, symlink escape, non-regular file, or wrong session:
			// reject immediately without waiting.
			return CompletedTurn{}
		}
		if !time.Now().Before(deadline) {
			return CompletedTurn{TranscriptPath: validatedPath}
		}
		time.Sleep(agyTranscriptPoll)
	}
}

func isTransientMissing(rawPath string) bool {
	path := strings.TrimSpace(rawPath)
	if path == "" || !filepath.IsAbs(path) || !strings.EqualFold(filepath.Ext(path), ".jsonl") {
		return false
	}
	_, err := os.Lstat(path)
	return os.IsNotExist(err)
}

// parseAgyTranscriptTail reads the bounded tail of an AGY JSONL transcript and
// resolves the latest completed turn. settled is true when the latest turn has
// reached a terminal state (completed response or explicit error/no-response
// boundary), false when the latest step is still RUNNING or the file is empty.
func parseAgyTranscriptTail(path string) (CompletedTurn, bool) {
	lines := readAgyTranscriptTail(path)
	if len(lines) == 0 {
		return CompletedTurn{}, false
	}

	var steps []agyTranscriptStep
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var step agyTranscriptStep
		if err := json.Unmarshal(line, &step); err != nil {
			continue
		}
		if step.Type == "" {
			continue
		}
		steps = append(steps, step)
	}
	if len(steps) == 0 {
		return CompletedTurn{}, false
	}

	// Walk backwards from the latest step to locate the final PLANNER_RESPONSE
	// of the current turn without crossing a USER_INPUT boundary.
	finalIdx := -1
	for i := len(steps) - 1; i >= 0; i-- {
		step := steps[i]
		switch step.Type {
		case "USER_INPUT":
			// Reached the start of the current turn without a completed final
			// assistant response.
			return CompletedTurn{}, false
		case "PLANNER_RESPONSE":
			status := strings.ToUpper(strings.TrimSpace(step.Status))
			if status == "RUNNING" || status == "PENDING" || status == "IN_PROGRESS" {
				return CompletedTurn{}, false
			}
			if status != "DONE" {
				// Failed or canceled turn: settled with no final response so we
				// never attribute an older turn's response.
				return CompletedTurn{}, true
			}
			if len(step.ToolCalls) > 0 {
				// The latest planner step is still a tool-call step; the final
				// text response may still be flushing.
				return CompletedTurn{}, false
			}
			content := strings.TrimSpace(step.Content)
			if content == "" {
				return CompletedTurn{}, true
			}
			finalIdx = i
		case "GENERIC":
			status := strings.ToUpper(strings.TrimSpace(step.Status))
			if status == "RUNNING" {
				return CompletedTurn{}, false
			}
		}
		if finalIdx >= 0 {
			break
		}
	}
	if finalIdx < 0 {
		return CompletedTurn{}, false
	}

	finalStep := steps[finalIdx]
	turn := CompletedTurn{
		FinalAssistantMessage: strings.TrimSpace(finalStep.Content),
	}
	if finalStep.StepIndex > 0 {
		turn.ProviderTurnID = fmt.Sprintf("step:%d", finalStep.StepIndex)
	}

	// Find the matching USER_INPUT that opened this turn.
	for i := finalIdx - 1; i >= 0; i-- {
		if steps[i].Type == "USER_INPUT" {
			if req := strings.TrimSpace(steps[i].Content); req != "" {
				turn.TurnUserRequest = req
			}
			break
		}
	}
	return turn, true
}

func readAgyTranscriptTail(path string) [][]byte {
	file, err := os.Open(path) //nolint:gosec // validated against trusted AGY storage root
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	offset := max(info.Size()-agyTranscriptTailBytes, 0)
	tail, err := io.ReadAll(io.NewSectionReader(file, offset, info.Size()-offset))
	if err != nil {
		return nil
	}
	lines := bytes.Split(tail, []byte("\n"))
	if offset > 0 && len(lines) > 0 {
		// Tail window may start mid-line.
		lines = lines[1:]
	}
	return lines
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
