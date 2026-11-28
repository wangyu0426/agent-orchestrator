-- +goose Up
-- +goose StatementBegin
ALTER TABLE review_run ADD COLUMN github_review_request_fingerprint TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE review_run DROP COLUMN github_review_request_fingerprint;
-- +goose StatementEnd
