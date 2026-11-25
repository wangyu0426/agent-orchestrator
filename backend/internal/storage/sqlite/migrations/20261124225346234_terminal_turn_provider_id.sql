-- +goose Up
ALTER TABLE session_terminal_turns ADD COLUMN provider_turn_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_session_terminal_turns_provider_turn
ON session_terminal_turns (session_id, harness, native_session_id, provider_turn_id)
WHERE provider_turn_id <> '';

-- +goose Down
DROP INDEX IF EXISTS idx_session_terminal_turns_provider_turn;
ALTER TABLE session_terminal_turns DROP COLUMN provider_turn_id;
