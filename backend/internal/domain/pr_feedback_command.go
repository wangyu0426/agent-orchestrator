package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// PRCommentFeedbackMarker is the exact first-line command marker required to
// treat a top-level PR conversation comment as explicit operator review
// feedback.
const PRCommentFeedbackMarker = "/ao review-feedback"

// PRCommentFeedbackSourceType identifies the durable source type for
// conversation-comment feedback rows.
const PRCommentFeedbackSourceType = "pr_conversation_comment"

// PRConversationFeedbackState represents the lifecycle state of a persisted
// conversation feedback command.
type PRConversationFeedbackState string

const (
	PRConversationFeedbackActive     PRConversationFeedbackState = "active"
	PRConversationFeedbackBaseline   PRConversationFeedbackState = "baseline"
	PRConversationFeedbackDeleted    PRConversationFeedbackState = "deleted"
	PRConversationFeedbackSuperseded PRConversationFeedbackState = "superseded"
)

// PRCommentFeedbackConfig controls opt-in ingestion of explicit top-level PR
// conversation feedback commands (`/ao review-feedback`).
type PRCommentFeedbackConfig struct {
	// Enabled opts this project in to ingesting explicit top-level PR
	// conversation feedback commands. Default is false (off).
	Enabled bool `json:"enabled,omitempty"`
	// TrustedAuthors lists explicit provider logins (matched case-insensitively)
	// authorized to issue top-level PR feedback commands. Empty means no author
	// is trusted.
	TrustedAuthors []string `json:"trustedAuthors,omitempty"`
	// EnabledAt is an optional UTC timestamp (RFC3339) establishing a baseline
	// cutoff: comments created before EnabledAt are treated as historic baseline
	// and never replayed.
	EnabledAt *time.Time `json:"enabledAt,omitempty"`
}

// Validate checks PRCommentFeedbackConfig fields.
func (c PRCommentFeedbackConfig) Validate() error {
	for i, a := range c.TrustedAuthors {
		trimmed := strings.TrimSpace(a)
		if trimmed == "" || trimmed != a || strings.ContainsAny(a, " \t\r\n") {
			return fmt.Errorf("prCommentFeedback.trustedAuthors[%d]: invalid author login %q", i, a)
		}
	}
	return nil
}

// ParsedPRFeedbackCommand is the normalized result of parsing a top-level PR
// conversation comment that starts with PRCommentFeedbackMarker.
type ParsedPRFeedbackCommand struct {
	Body     string
	BodyHash string
}

// ParsePRFeedbackCommand parses a top-level PR conversation comment body for
// the exact first-line marker `/ao review-feedback` followed by nonempty
// actionable text.
//
// It rejects:
//   - arbitrary prose without the marker ("no_marker")
//   - spoofed markers inside quotes (`>`), code fences, inline code, or later lines ("spoofed_marker")
//   - marker with no actionable text ("empty_body")
func ParsePRFeedbackCommand(raw string) (ParsedPRFeedbackCommand, bool, string) {
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) == 0 {
		return ParsedPRFeedbackCommand{}, false, "no_marker"
	}
	firstLine := lines[0]

	if !strings.HasPrefix(firstLine, PRCommentFeedbackMarker) {
		if strings.Contains(normalized, PRCommentFeedbackMarker) {
			return ParsedPRFeedbackCommand{}, false, "spoofed_marker"
		}
		return ParsedPRFeedbackCommand{}, false, "no_marker"
	}

	restOfFirst := firstLine[len(PRCommentFeedbackMarker):]
	if restOfFirst != "" && restOfFirst[0] != ' ' && restOfFirst[0] != '\t' {
		// e.g. "/ao review-feedbacks" is not the exact marker.
		return ParsedPRFeedbackCommand{}, false, "no_marker"
	}

	var parts []string
	if trimmedFirst := strings.TrimSpace(restOfFirst); trimmedFirst != "" {
		parts = append(parts, trimmedFirst)
	}
	if len(lines) > 1 {
		bodyRest := strings.TrimSpace(strings.Join(lines[1:], "\n"))
		if bodyRest != "" {
			parts = append(parts, bodyRest)
		}
	}
	actionable := strings.TrimSpace(strings.Join(parts, "\n"))
	if actionable == "" {
		return ParsedPRFeedbackCommand{}, false, "empty_body"
	}

	sum := sha256.Sum256([]byte(actionable))
	return ParsedPRFeedbackCommand{
		Body:     actionable,
		BodyHash: hex.EncodeToString(sum[:]),
	}, true, ""
}

// AuthorizePRCommentFeedback checks whether a top-level PR comment from the
// given provider-supplied author identity is authorized under cfg.
//
// Bots (including AO's own GitHub App) and unknown/empty actors are always
// rejected, even if their login appears in TrustedAuthors. A trusted human
// operator who is also the PR author IS permitted because human connectors and
// workers may share a GitHub account.
func AuthorizePRCommentFeedback(cfg PRCommentFeedbackConfig, author string, isBot bool) (bool, string) {
	if !cfg.Enabled {
		return false, "feature_disabled"
	}
	if isBot {
		return false, "bot_author"
	}
	login := strings.TrimSpace(author)
	if login == "" {
		return false, "unknown_author"
	}
	for _, trusted := range cfg.TrustedAuthors {
		if strings.EqualFold(strings.TrimSpace(trusted), login) {
			return true, ""
		}
	}
	return false, "untrusted_author"
}
