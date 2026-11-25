package review

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestCrossHarnessHeadCoveredBlocksDuplicateOnMultiPR(t *testing.T) {
	runs := []domain.ReviewRun{
		{PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha-1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved},
		{PRURL: "https://github.com/o/r/pull/2", TargetSHA: "sha-2", Harness: domain.ReviewerCodex, Status: domain.ReviewRunFailed, TriggerSource: domain.ReviewTriggerAuto},
	}
	if !crossHarnessHeadCovered(runs, "https://github.com/o/r/pull/1", "sha-1") {
		t.Fatal("PR #1 already approved by Codex must be covered across harnesses")
	}
	if crossHarnessHeadCovered(runs, "https://github.com/o/r/pull/2", "sha-2") {
		t.Fatal("PR #2 only has a failed Codex run and must remain eligible for fallback")
	}
	if crossHarnessHeadCovered(runs, "https://github.com/o/r/pull/1", "sha-new") {
		t.Fatal("PR #1 with a new HEAD sha must remain eligible")
	}
}
