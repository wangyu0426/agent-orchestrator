-- +goose Up
-- +goose StatementBegin
ALTER TABLE review_run ADD COLUMN github_review_state TEXT NOT NULL DEFAULT '';
ALTER TABLE review_run ADD COLUMN github_review_author TEXT NOT NULL DEFAULT '';
ALTER TABLE review_run ADD COLUMN github_review_commit_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE review_run ADD COLUMN github_review_error TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE review_run DROP COLUMN github_review_error;
ALTER TABLE review_run DROP COLUMN github_review_commit_sha;
ALTER TABLE review_run DROP COLUMN github_review_author;
ALTER TABLE review_run DROP COLUMN github_review_state;
-- +goose StatementEnd
