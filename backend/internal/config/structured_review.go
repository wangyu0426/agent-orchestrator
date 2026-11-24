package config

import (
	"os"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/structuredreview"
)

func loadStructuredReview() (structuredreview.Policy, error) {
	var policy structuredreview.Policy
	if raw := os.Getenv("AO_STRUCTURED_REVIEW_ENABLED"); raw != "" {
		v, err := parseToggleEnv("AO_STRUCTURED_REVIEW_ENABLED", raw)
		if err != nil {
			return structuredreview.Policy{}, err
		}
		policy.Enabled = v
	}
	if raw := os.Getenv("AO_STRUCTURED_REVIEW_ALLOW_PR_AUTHOR"); raw != "" {
		v, err := parseToggleEnv("AO_STRUCTURED_REVIEW_ALLOW_PR_AUTHOR", raw)
		if err != nil {
			return structuredreview.Policy{}, err
		}
		policy.AllowPRAuthor = v
	}
	if raw := os.Getenv("AO_STRUCTURED_REVIEW_TRUSTED_REVIEWERS"); strings.TrimSpace(raw) != "" {
		for _, part := range strings.Split(raw, ",") {
			login := strings.TrimPrefix(strings.TrimSpace(part), "@")
			if login != "" {
				policy.TrustedReviewers = append(policy.TrustedReviewers, login)
			}
		}
	}
	return policy, nil
}
