-- +goose Up
-- Temporary model escalation (issue #8): one JSON record per session plus
-- a mirrored state column so transitions can CAS on state and boot
-- recovery can list active overrides without parsing JSON. NULL means
-- the session never had an escalation.
ALTER TABLE sessions ADD COLUMN temporary_escalation TEXT;
ALTER TABLE sessions ADD COLUMN temporary_escalation_state TEXT;
CREATE INDEX idx_sessions_temporary_escalation_state
    ON sessions(temporary_escalation_state)
    WHERE temporary_escalation_state IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_sessions_temporary_escalation_state;
ALTER TABLE sessions DROP COLUMN temporary_escalation_state;
ALTER TABLE sessions DROP COLUMN temporary_escalation;
