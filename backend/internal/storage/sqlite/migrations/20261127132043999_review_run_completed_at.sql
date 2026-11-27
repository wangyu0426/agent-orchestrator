-- +goose Up
ALTER TABLE review_run ADD COLUMN completed_at TIMESTAMP;

-- +goose Down
ALTER TABLE review_run DROP COLUMN completed_at;
