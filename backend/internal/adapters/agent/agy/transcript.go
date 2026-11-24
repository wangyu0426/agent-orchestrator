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
// file inside the session-owned brain/<conversationID>/ directory under a
// trusted AGY storage root in GEMINI_HOME (without symlink traversal outside
// that directory). Both a valid conversationID and a matching brain/<id>/
// layout are required; missing IDs or unrecognized layouts fail closed.
func ValidateTranscriptPath(rawPath, conversationID string) (string, bool) {
	resolvedPath, _, ok := validateTranscriptPathInfo(rawPath, conversationID)
	return resolvedPath, ok
}

func validateTranscriptPathInfo(rawPath, conversationID string) (string, os.FileInfo, bool) {
	path := strings.TrimSpace(rawPath)
	convID := strings.TrimSpace(conversationID)
	if path == "" || !filepath.IsAbs(path) || !validConversationID(convID) {
		return "", nil, false
	}
	if !strings.EqualFold(filepath.Ext(path), ".jsonl") {
		return "", nil, false
	}
	geminiHome, err := GeminiHome(nil)
	if err != nil || strings.TrimSpace(geminiHome) == "" {
		return "", nil, false
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, false
	}
	info, err := os.Stat(resolvedPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, false
	}
	if !isWithinOwnedBrainDir(resolvedPath, geminiHome, convID) {
		return "", nil, false
	}
	return resolvedPath, info, true
}

func isWithinOwnedBrainDir(resolvedPath, geminiHome, convID string) bool {
	trustedDirs := []string{
		filepath.Join(geminiHome, CLIStorageDir, "brain", convID),
		filepath.Join(geminiHome, ACPStorageDir, "brain", convID),
		filepath.Join(geminiHome, "antigravity", "brain", convID),
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

// ResolveStopTranscript validates the transcriptPath in an AGY Stop hook
// payload and extracts the completed turn's final assistant response and
// matching user request. It never returns an earlier turn's response when the
// latest turn is incomplete, failed, or has a malformed tail boundary, and
// never reads outside the session-owned brain directory.
func ResolveStopTranscript(payload []byte) CompletedTurn {
	var in agyStopPayload
	if err := json.Unmarshal(payload, &in); err != nil {
		return CompletedTurn{}
	}
	rawPath := firstNonEmpty(in.TranscriptPath, in.TranscriptPathCamel)
	convID := firstNonEmpty(in.ConversationID, in.ConversationIDCamel, in.SessionID, in.SessionIDCamel)
	if rawPath == "" || !validConversationID(convID) {
		return CompletedTurn{}
	}

	deadline := time.Now().Add(agyTranscriptWait)
	var lastSize int64 = -1
	var validatedPath string

	for {
		path, info, ok := validateTranscriptPathInfo(rawPath, convID)
		if ok {
			validatedPath = path
			if info.Size() != lastSize {
				lastSize = info.Size()
				turn, settled := parseAgyTranscriptTail(path, info)
				turn.TranscriptPath = path
				if settled || turn.FinalAssistantMessage != "" {
					return turn
				}
			}
		} else if !isTransientMissing(rawPath) {
			// Unsafe path, symlink escape, non-regular file, missing ID, or
			// wrong session/layout: reject immediately without waiting.
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
// resolves the latest completed turn. It scans backwards from the end and fails
// closed if a malformed line appears at the tail before a completed
// PLANNER_RESPONSE, preventing partial newer writes from falling back to an
// earlier turn.
func parseAgyTranscriptTail(path string, expectedInfo os.FileInfo) (CompletedTurn, bool) {
	lines := readAgyTranscriptTail(path, expectedInfo)
	if len(lines) == 0 {
		return CompletedTurn{}, false
	}

	finalLineIdx := -1
	var finalStep agyTranscriptStep
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var step agyTranscriptStep
		if err := json.Unmarshal(line, &step); err != nil || step.Type == "" {
			// Malformed or partial JSON at the tail boundary: fail closed so
			// we never silently attribute an older completed turn.
			return CompletedTurn{}, false
		}
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
			finalLineIdx = i
			finalStep = step
		case "GENERIC":
			status := strings.ToUpper(strings.TrimSpace(step.Status))
			if status == "RUNNING" {
				return CompletedTurn{}, false
			}
		}
		if finalLineIdx >= 0 {
			break
		}
	}
	if finalLineIdx < 0 {
		return CompletedTurn{}, false
	}

	turn := CompletedTurn{
		FinalAssistantMessage: strings.TrimSpace(finalStep.Content),
	}
	if finalStep.StepIndex > 0 {
		turn.ProviderTurnID = fmt.Sprintf("step:%d", finalStep.StepIndex)
	}

	// Find the matching USER_INPUT that opened this turn.
	for i := finalLineIdx - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var step agyTranscriptStep
		if err := json.Unmarshal(line, &step); err != nil {
			continue
		}
		if step.Type == "USER_INPUT" {
			if req := strings.TrimSpace(step.Content); req != "" {
				turn.TurnUserRequest = req
			}
			break
		}
	}
	return turn, true
}

func readAgyTranscriptTail(path string, expectedInfo os.FileInfo) [][]byte {
	file, err := os.Open(path) //nolint:gosec // validated against session-owned AGY brain directory
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	if expectedInfo != nil && !os.SameFile(expectedInfo, info) {
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
