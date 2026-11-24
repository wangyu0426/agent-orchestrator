-- +goose Up
-- New worker sessions archive after their PRs merge unless the user or
-- project opts out. Existing session rows keep their persisted policy.
ALTER TABLE app_settings ADD COLUMN terminate_on_pr_merge_default BOOLEAN NOT NULL DEFAULT TRUE;

-- +goose Down
ALTER TABLE app_settings DROP COLUMN terminate_on_pr_merge_default;
