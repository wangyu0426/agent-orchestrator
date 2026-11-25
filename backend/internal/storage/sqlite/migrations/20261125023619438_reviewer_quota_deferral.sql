-- +goose Up

CREATE TABLE reviewer_quota_deferral (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    pr_url TEXT NOT NULL,
    target_sha TEXT NOT NULL,
    retry_at INTEGER NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, pr_url, target_sha)
);

-- +goose Down

DROP TABLE reviewer_quota_deferral;
