package sqlite_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerquota"
)

func TestWorkerQuotaAdmissionAndAttemptSurviveRestart(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	st, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	rec, err := st.CreateSession(ctx, domain.SessionRecord{
		Kind: domain.KindWorker, Harness: domain.HarnessCodex, WorkerQuotaManaged: true,
		CreatedAt: now, UpdatedAt: now, Metadata: domain.SessionMetadata{Model: "codex-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var claimed atomic.Int32
	var attempts sync.WaitGroup
	for range 8 {
		attempts.Go(func() {
			ok, err := st.ClaimWorkerQuotaAttempt(ctx, rec.ID, "quota:one", workerquota.Target{Harness: domain.HarnessAgy})
			if err != nil {
				t.Error(err)
			}
			if ok {
				claimed.Add(1)
			}
		})
	}
	attempts.Wait()
	if claimed.Load() != 1 {
		t.Fatalf("claimed %d handoffs", claimed.Load())
	}
	if err := st.RecordWorkerQuotaFailure(ctx, rec.ID, workerquota.Failure{
		Target: workerquota.Target{Harness: domain.HarnessCodex, Model: "codex-model"},
		Scope:  "computer login", At: now, ResetAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	sessions, err := restored.WorkerQuotaSessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].ID != rec.ID || sessions[0].Attempt != "quota:one" {
		t.Fatalf("restored sessions %+v, %v", sessions, err)
	}
	if ok, err := restored.ClaimWorkerQuotaAttempt(ctx, rec.ID, "quota:two", workerquota.Target{Harness: domain.HarnessAgy}); err != nil || ok {
		t.Fatalf("duplicate attempt = %v, %v", ok, err)
	}
	if f, err := restored.WorkerQuotaFailures(ctx, now); err != nil || len(f) != 1 || !f[0].At.Equal(now) {
		t.Fatalf("restored evidence %+v, %v", f, err)
	}
	if f, err := restored.WorkerQuotaFailures(ctx, now.Add(time.Minute)); err != nil || len(f) != 0 {
		t.Fatalf("expired evidence %+v, %v", f, err)
	}
}
