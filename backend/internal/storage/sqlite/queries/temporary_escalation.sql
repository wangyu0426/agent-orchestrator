-- Temporary model escalation (issue #8). The record is one JSON blob on
-- the sessions row; its state is mirrored in a column so transitions can
-- CAS on state and boot recovery can list active overrides. NULL columns
-- mean the session never had an escalation.

-- name: GetTemporaryEscalation :one
SELECT id, temporary_escalation, temporary_escalation_state
FROM sessions
WHERE id = ?;

-- name: CreateTemporaryEscalation :execrows
-- Claims the escalation slot only when no active or failed escalation
-- exists for the session; a terminal record may be replaced by a new one.
UPDATE sessions
SET temporary_escalation = ?, temporary_escalation_state = ?, updated_at = ?
WHERE id = ?
  AND (temporary_escalation_state IS NULL
    OR temporary_escalation_state IN ('reverted', 'superseded', 'cancelled'));

-- name: UpdateTemporaryEscalation :execrows
-- Compare-and-swap on the previous state: exactly one writer can move a
-- record out of a state, which is what makes the automatic restore
-- happen exactly once.
UPDATE sessions
SET temporary_escalation = ?, temporary_escalation_state = ?, updated_at = ?
WHERE id = ? AND temporary_escalation_state = ?;

-- name: DeleteTemporaryEscalation :exec
UPDATE sessions
SET temporary_escalation = NULL, temporary_escalation_state = NULL
WHERE id = ?;

-- name: ListActiveTemporaryEscalations :many
SELECT id, temporary_escalation, temporary_escalation_state
FROM sessions
WHERE temporary_escalation_state IN ('pending', 'awaiting_idle', 'reverting', 'failed')
ORDER BY id;
