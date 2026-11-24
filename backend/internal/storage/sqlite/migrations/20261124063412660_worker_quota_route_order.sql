-- +goose Up

ALTER TABLE worker_quota_session ADD COLUMN remaining_routes TEXT NOT NULL DEFAULT '[]';

-- +goose Down

ALTER TABLE worker_quota_session DROP COLUMN remaining_routes;
