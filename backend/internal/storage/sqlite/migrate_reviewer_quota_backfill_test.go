package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestMigrateReviewerQuotaBackfillReclassifiesOnlyQuotaExhaustedRuns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pre_upgrade.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Migrate up to the version right before 20261125023619438_reviewer_quota_deferral.sql.
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, "migrations", 20261124063412660); err != nil {
		t.Fatalf("goose.UpTo pre-upgrade: %v", err)
	}

	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	stmts := []string{
		`INSERT INTO projects (id, path, registered_at) VALUES ('p', '/tmp/p', '2026-11-25T12:00:00Z')`,
		`INSERT INTO sessions (id, project_id, num, kind, harness, activity_last_at, created_at, updated_at) VALUES ('s-1', 'p', 1, 'worker', 'codex', '2026-11-25T12:00:00Z', '2026-11-25T12:00:00Z', '2026-11-25T12:00:00Z')`,
		`INSERT INTO review (id, session_id, project_id, harness, interface_mode, created_at, updated_at) VALUES ('rev-1', 's-1', 'p', 'codex', 'chat', '2026-11-25T12:00:00Z', '2026-11-25T12:00:00Z')`,
		`INSERT INTO conversations (id, scope, project_id, review_id, created_at, updated_at) VALUES ('conv-1', 'review', 'p', 'rev-1', '2026-11-25T12:00:00Z', '2026-11-25T12:00:00Z')`,
		// Quota-failed turn + daemon message + pre-upgrade generic failed review_run.
		`INSERT INTO conversation_turns (id, conversation_id, handled_by_session_id, handled_by_review_id, state, failure_code, requested_at, completed_at) VALUES ('turn-quota', 'conv-1', 's-1', 'rev-1', 'failed', 'quota_exhausted', '2026-11-25T12:00:00Z', '2026-11-25T12:01:00Z')`,
		`INSERT INTO conversation_messages (id, conversation_id, turn_id, sequence, role, origin, client_message_id, text, created_at, updated_at) VALUES ('msg-quota', 'conv-1', 'turn-quota', 1, 'user', 'daemon', 'review-batch:batch-quota', 'review', '2026-11-25T12:00:00Z', '2026-11-25T12:00:00Z')`,
		`INSERT INTO review_run (id, review_id, session_id, harness, pr_url, target_sha, batch_id, status, verdict, body, trigger_source, created_at) VALUES ('run-quota', 'rev-1', 's-1', 'codex', 'https://github.com/o/r/pull/1', 'sha-1', 'batch-quota', 'failed', '', 'reviewer Chat turn ended without submitting a result', 'auto', '2026-11-25T12:00:00Z')`,
		// Ordinary non-quota failed turn + daemon message + failed review_run.
		`INSERT INTO conversation_turns (id, conversation_id, handled_by_session_id, handled_by_review_id, state, failure_code, requested_at, completed_at) VALUES ('turn-other', 'conv-1', 's-1', 'rev-1', 'failed', '', '2026-11-25T12:02:00Z', '2026-11-25T12:03:00Z')`,
		`INSERT INTO conversation_messages (id, conversation_id, turn_id, sequence, role, origin, client_message_id, text, created_at, updated_at) VALUES ('msg-other', 'conv-1', 'turn-other', 2, 'user', 'daemon', 'review-batch:batch-other', 'review', '2026-11-25T12:02:00Z', '2026-11-25T12:02:00Z')`,
		`INSERT INTO review_run (id, review_id, session_id, harness, pr_url, target_sha, batch_id, status, verdict, body, trigger_source, created_at) VALUES ('run-other', 'rev-1', 's-1', 'codex', 'https://github.com/o/r/pull/1', 'sha-1', 'batch-other', 'failed', '', 'reviewer Chat turn ended without submitting a result', 'auto', '2026-11-25T12:02:00Z')`,
	}
	_ = now
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// Upgrade to latest migration (includes 20261125023619438_reviewer_quota_deferral.sql).
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to latest: %v", err)
	}

	var quotaBody, otherBody string
	if err := db.QueryRow(`SELECT body FROM review_run WHERE id = 'run-quota'`).Scan(&quotaBody); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT body FROM review_run WHERE id = 'run-other'`).Scan(&otherBody); err != nil {
		t.Fatal(err)
	}
	if quotaBody != domain.ReviewRunQuotaExhaustedBody {
		t.Fatalf("run-quota body = %q, want %q", quotaBody, domain.ReviewRunQuotaExhaustedBody)
	}
	if otherBody != "reviewer Chat turn ended without submitting a result" {
		t.Fatalf("run-other body = %q, want generic body preserved", otherBody)
	}
}
