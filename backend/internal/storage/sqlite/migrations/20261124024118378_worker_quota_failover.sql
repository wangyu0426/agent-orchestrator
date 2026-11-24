-- +goose Up

CREATE TABLE worker_quota_session (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    harness TEXT NOT NULL,
    model TEXT NOT NULL,
    effort TEXT NOT NULL,
    -- At most one automatic handoff attempt per session, including across restart.
    attempt_owner TEXT NOT NULL DEFAULT '',
    target_harness TEXT NOT NULL DEFAULT '',
    target_model TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    failure_at INTEGER NOT NULL DEFAULT 0,
    reset_at INTEGER NOT NULL DEFAULT 0,
    failure_harness TEXT NOT NULL DEFAULT '',
    failure_model TEXT NOT NULL DEFAULT ''
);

CREATE TABLE worker_quota_automation_deferral (
    run_id TEXT PRIMARY KEY REFERENCES automation_runs(id) ON DELETE CASCADE,
    retry_at DATETIME NOT NULL
);

-- +goose Down

DROP TABLE worker_quota_automation_deferral;

DROP TABLE worker_quota_session;
