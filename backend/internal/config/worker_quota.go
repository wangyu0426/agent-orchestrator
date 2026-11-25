package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

func loadWorkerQuota() (*workerquota.Policy, error) {
	quotas, err := loadRoleQuotas()
	if err != nil {
		return nil, err
	}
	return quotas.Worker, nil
}

func loadRoleQuotas() (workerquota.RolePolicies, error) {
	var policies workerquota.RolePolicies
	var err error
	if policies.Worker, err = loadRoleQuotaPolicy(
		workerquota.RoleWorker,
		"AO_WORKER_QUOTA_FAILOVER",
		"AO_WORKER_QUOTA_PRIMARY",
		"AO_WORKER_QUOTA_FALLBACKS",
		validateWorkerQuotaTarget,
	); err != nil {
		return workerquota.RolePolicies{}, err
	}
	if policies.Reviewer, err = loadRoleQuotaPolicy(
		workerquota.RoleReviewer,
		"AO_REVIEWER_QUOTA_FAILOVER",
		"AO_REVIEWER_QUOTA_PRIMARY",
		"AO_REVIEWER_QUOTA_FALLBACKS",
		validateReviewerQuotaTarget,
	); err != nil {
		return workerquota.RolePolicies{}, err
	}
	if policies.Orchestrator, err = loadRoleQuotaPolicy(
		workerquota.RoleOrchestrator,
		"AO_ORCHESTRATOR_QUOTA_FAILOVER",
		"AO_ORCHESTRATOR_QUOTA_PRIMARY",
		"AO_ORCHESTRATOR_QUOTA_FALLBACKS",
		validateWorkerQuotaTarget,
	); err != nil {
		return workerquota.RolePolicies{}, err
	}
	if !policies.AnyEnabled() {
		return policies, nil
	}
	if base := os.Getenv("AO_QUOTUM_URL"); base != "" {
		var bindings map[domain.AgentHarness]workerquota.Binding
		if err := workerQuotaJSON("AO_QUOTUM_BINDINGS", &bindings); err != nil {
			return workerquota.RolePolicies{}, err
		}
		if len(bindings) == 0 {
			return workerquota.RolePolicies{}, fmt.Errorf("AO_QUOTUM_BINDINGS is required with AO_QUOTUM_URL")
		}
		session, err := readQuotumSession(os.Getenv("AO_QUOTUM_SESSION_FILE"))
		if err != nil {
			return workerquota.RolePolicies{}, err
		}
		source, err := workerquota.NewQuotum(base, os.Getenv("AO_QUOTUM_BOARD"), session, bindings)
		if err != nil {
			return workerquota.RolePolicies{}, err
		}
		for _, p := range []*workerquota.Policy{policies.Worker, policies.Reviewer, policies.Orchestrator} {
			if p != nil {
				p.Source = source
			}
		}
	}
	return policies, nil
}

func loadRoleQuotaPolicy(
	role workerquota.Role,
	toggleEnv, primaryEnv, fallbacksEnv string,
	validate func(string, workerquota.Target) error,
) (*workerquota.Policy, error) {
	raw := strings.TrimSpace(os.Getenv(toggleEnv))
	if raw == "" {
		return nil, nil
	}
	enabled, err := parseToggleEnv(toggleEnv, raw)
	if err != nil || !enabled {
		return nil, err
	}
	policy := &workerquota.Policy{Role: role}
	if err := workerQuotaJSON(primaryEnv, &policy.Primary); err != nil {
		return nil, err
	}
	if err := workerQuotaJSON(fallbacksEnv, &policy.Fallbacks); err != nil {
		return nil, err
	}
	if len(policy.Fallbacks) == 0 || len(policy.Fallbacks) > 8 {
		return nil, fmt.Errorf("%s must specify one to eight targets", fallbacksEnv)
	}
	if policy.Primary != nil {
		if err := validate(primaryEnv, *policy.Primary); err != nil {
			return nil, err
		}
	}
	for _, target := range policy.Fallbacks {
		if err := validate(fallbacksEnv, target); err != nil {
			return nil, err
		}
	}
	return policy, nil
}

func validateWorkerQuotaTarget(source string, target workerquota.Target) error {
	if !target.Harness.IsKnown() {
		return fmt.Errorf("%s requires a known harness for every target", source)
	}
	if target.Model != strings.TrimSpace(target.Model) || target.Effort != strings.TrimSpace(target.Effort) || target.Account != strings.TrimSpace(target.Account) {
		return fmt.Errorf("%s requires trimmed model, effort, and account values", source)
	}
	mode, err := domain.ParseSessionMode(string(target.Mode))
	if err != nil {
		return fmt.Errorf("%s: %w", source, err)
	}
	if !target.Harness.SupportsMode(mode) {
		return fmt.Errorf("%s: harness %q does not support mode %q", source, target.Harness, mode)
	}
	return nil
}

func validateReviewerQuotaTarget(source string, target workerquota.Target) error {
	if !domain.ReviewerHarness(target.Harness).IsKnown() {
		return fmt.Errorf("%s requires a known reviewer harness for every target", source)
	}
	if target.Model != strings.TrimSpace(target.Model) || target.Effort != strings.TrimSpace(target.Effort) || target.Account != strings.TrimSpace(target.Account) {
		return fmt.Errorf("%s requires trimmed model, effort, and account values", source)
	}
	if _, err := domain.ParseSessionMode(string(target.Mode)); err != nil {
		return fmt.Errorf("%s: %w", source, err)
	}
	return nil
}

func workerQuotaJSON(name string, value any) error {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("%s contains invalid JSON", name)
	}
	return nil
}

func readQuotumSession(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot read AO_QUOTUM_SESSION_FILE")
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(raw) > 4096 {
		return "", fmt.Errorf("invalid AO_QUOTUM_SESSION_FILE contents")
	}
	return strings.TrimSpace(string(raw)), nil
}
