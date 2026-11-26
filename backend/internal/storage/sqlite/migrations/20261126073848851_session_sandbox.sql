-- +goose Up
ALTER TABLE sessions ADD COLUMN session_sandbox TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN session_sandbox;
