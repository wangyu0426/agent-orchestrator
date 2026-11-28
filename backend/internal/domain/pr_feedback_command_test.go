package domain

import "testing"

func TestParsePRFeedbackCommand(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantOK     bool
		wantBody   string
		wantReason string
	}{
		{
			name:     "inline actionable text",
			body:     "/ao review-feedback Please add a regression test for empty input.",
			wantOK:   true,
			wantBody: "Please add a regression test for empty input.",
		},
		{
			name:     "multiline actionable text",
			body:     "/ao review-feedback\nFix the race condition in observer.go\nand add a unit test.",
			wantOK:   true,
			wantBody: "Fix the race condition in observer.go\nand add a unit test.",
		},
		{
			name:     "crlf normalization",
			body:     "/ao review-feedback\r\nLine 1\r\nLine 2\r\n",
			wantOK:   true,
			wantBody: "Line 1\nLine 2",
		},
		{
			name:       "unmarked prose",
			body:       "Looks good overall, please fix the typo.",
			wantOK:     false,
			wantReason: "no_marker",
		},
		{
			name:       "empty body after marker",
			body:       "/ao review-feedback   \n  \n",
			wantOK:     false,
			wantReason: "empty_body",
		},
		{
			name:       "marker with trailing suffix",
			body:       "/ao review-feedbacks please fix",
			wantOK:     false,
			wantReason: "no_marker",
		},
		{
			name:       "spoofed inside quote",
			body:       "> /ao review-feedback please fix",
			wantOK:     false,
			wantReason: "spoofed_marker",
		},
		{
			name:       "spoofed inside code fence",
			body:       "```\n/ao review-feedback please fix\n```",
			wantOK:     false,
			wantReason: "spoofed_marker",
		},
		{
			name:       "spoofed inside inline code",
			body:       "`/ao review-feedback` please fix",
			wantOK:     false,
			wantReason: "spoofed_marker",
		},
		{
			name:       "spoofed on second line",
			body:       "Please ignore this:\n/ao review-feedback do something",
			wantOK:     false,
			wantReason: "spoofed_marker",
		},
		{
			name:       "spoofed with leading spaces",
			body:       "  /ao review-feedback indented",
			wantOK:     false,
			wantReason: "spoofed_marker",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, reason := ParsePRFeedbackCommand(tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (reason=%q)", ok, tc.wantOK, reason)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if ok {
				if got.Body != tc.wantBody {
					t.Fatalf("body = %q, want %q", got.Body, tc.wantBody)
				}
				if got.BodyHash == "" {
					t.Fatal("expected non-empty BodyHash")
				}
			}
		})
	}
}

func TestAuthorizePRCommentFeedback(t *testing.T) {
	cfg := PRCommentFeedbackConfig{
		Enabled:        true,
		TrustedAuthors: []string{"WangYu0426", "reviewer-alice"},
	}

	tests := []struct {
		name       string
		cfg        PRCommentFeedbackConfig
		author     string
		isBot      bool
		wantOK     bool
		wantReason string
	}{
		{
			name:       "disabled by default",
			cfg:        PRCommentFeedbackConfig{},
			author:     "wangyu0426",
			wantOK:     false,
			wantReason: "feature_disabled",
		},
		{
			name:   "trusted author case-insensitive",
			cfg:    cfg,
			author: "wangyu0426",
			wantOK: true,
		},
		{
			name:       "bot rejected even if login listed",
			cfg:        PRCommentFeedbackConfig{Enabled: true, TrustedAuthors: []string{"ao-daemon[bot]"}},
			author:     "ao-daemon[bot]",
			isBot:      true,
			wantOK:     false,
			wantReason: "bot_author",
		},
		{
			name:       "unknown empty author rejected",
			cfg:        cfg,
			author:     "  ",
			wantOK:     false,
			wantReason: "unknown_author",
		},
		{
			name:       "untrusted collaborator rejected",
			cfg:        cfg,
			author:     "random-user",
			wantOK:     false,
			wantReason: "untrusted_author",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := AuthorizePRCommentFeedback(tc.cfg, tc.author, tc.isBot)
			if ok != tc.wantOK || reason != tc.wantReason {
				t.Fatalf("got (%v, %q), want (%v, %q)", ok, reason, tc.wantOK, tc.wantReason)
			}
		})
	}
}
