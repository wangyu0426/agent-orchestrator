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
	raw := strings.TrimSpace(os.Getenv("AO_WORKER_QUOTA_FAILOVER"))
	if raw == "" {
		return nil, nil
	}
	enabled, err := parseToggleEnv("AO_WORKER_QUOTA_FAILOVER", raw)
	if err != nil || !enabled {
		return nil, err
	}
	policy := &workerquota.Policy{}
	if err := workerQuotaJSON("AO_WORKER_QUOTA_PRIMARY", &policy.Primary); err != nil {
		return nil, err
	}
	if err := workerQuotaJSON("AO_WORKER_QUOTA_FALLBACKS", &policy.Fallbacks); err != nil {
		return nil, err
	}
	if len(policy.Fallbacks) == 0 || len(policy.Fallbacks) > 8 {
		return nil, fmt.Errorf("AO_WORKER_QUOTA_FALLBACKS must specify one to eight targets")
	}
	for _, target := range policy.Targets(workerquota.Target{}) {
		if !target.Harness.IsKnown() || target.Model != strings.TrimSpace(target.Model) || target.Effort != strings.TrimSpace(target.Effort) {
			return nil, fmt.Errorf("worker quota targets require a known harness and trimmed model and effort values")
		}
	}
	for _, target := range policy.Fallbacks {
		if !target.Harness.IsKnown() {
			return nil, fmt.Errorf("AO_WORKER_QUOTA_FALLBACKS requires a known harness for every target")
		}
	}
	if policy.Primary != nil && !policy.Primary.Harness.IsKnown() {
		return nil, fmt.Errorf("AO_WORKER_QUOTA_PRIMARY requires a known harness")
	}
	if base := os.Getenv("AO_QUOTUM_URL"); base != "" {
		var bindings map[domain.AgentHarness]workerquota.Binding
		if err := workerQuotaJSON("AO_QUOTUM_BINDINGS", &bindings); err != nil {
			return nil, err
		}
		if len(bindings) == 0 {
			return nil, fmt.Errorf("AO_QUOTUM_BINDINGS is required with AO_QUOTUM_URL")
		}
		session, err := readQuotumSession(os.Getenv("AO_QUOTUM_SESSION_FILE"))
		if err != nil {
			return nil, err
		}
		source, err := workerquota.NewQuotum(base, os.Getenv("AO_QUOTUM_BOARD"), session, bindings)
		if err != nil {
			return nil, err
		}
		policy.Source = source
	}
	return policy, nil
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
