package config

import (
	"testing"
)

func TestLoadStructuredReview(t *testing.T) {
	t.Run("defaults disabled", func(t *testing.T) {
		t.Setenv("AO_STRUCTURED_REVIEW_ENABLED", "")
		t.Setenv("AO_STRUCTURED_REVIEW_ALLOW_PR_AUTHOR", "")
		t.Setenv("AO_STRUCTURED_REVIEW_TRUSTED_REVIEWERS", "")
		p, err := loadStructuredReview()
		if err != nil {
			t.Fatalf("loadStructuredReview() err = %v", err)
		}
		if p.Enabled || p.AllowPRAuthor || len(p.TrustedReviewers) != 0 {
			t.Fatalf("unexpected default policy: %+v", p)
		}
	})

	t.Run("parses opt-in and trusted reviewers", func(t *testing.T) {
		t.Setenv("AO_STRUCTURED_REVIEW_ENABLED", "true")
		t.Setenv("AO_STRUCTURED_REVIEW_ALLOW_PR_AUTHOR", "1")
		t.Setenv("AO_STRUCTURED_REVIEW_TRUSTED_REVIEWERS", " @alice, bob , , @carol ")
		p, err := loadStructuredReview()
		if err != nil {
			t.Fatalf("loadStructuredReview() err = %v", err)
		}
		if !p.Enabled || !p.AllowPRAuthor {
			t.Fatalf("expected enabled and allowPRAuthor, got %+v", p)
		}
		if len(p.TrustedReviewers) != 3 || p.TrustedReviewers[0] != "alice" || p.TrustedReviewers[1] != "bob" || p.TrustedReviewers[2] != "carol" {
			t.Fatalf("unexpected trusted reviewers: %+v", p.TrustedReviewers)
		}
	})

	t.Run("invalid toggle errors", func(t *testing.T) {
		t.Setenv("AO_STRUCTURED_REVIEW_ENABLED", "maybe")
		if _, err := loadStructuredReview(); err == nil {
			t.Fatalf("expected error for invalid AO_STRUCTURED_REVIEW_ENABLED")
		}
	})
}
