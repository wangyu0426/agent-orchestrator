-- +goose Up
ALTER TABLE pr_conversation_feedback
    ADD COLUMN unverified INTEGER NOT NULL DEFAULT 0 CHECK (unverified IN (0, 1));

-- +goose Down
ALTER TABLE pr_conversation_feedback DROP COLUMN unverified;
