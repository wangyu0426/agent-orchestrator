-- name: InsertWorkerQuotaSession :exec
INSERT INTO worker_quota_session (session_id, harness, model, effort, remaining_routes)
VALUES (?, ?, ?, ?, ?);

-- name: ListWorkerQuotaSessions :many
SELECT worker_quota_session.* FROM worker_quota_session
JOIN sessions ON sessions.id = worker_quota_session.session_id
WHERE NOT sessions.is_terminated;

-- name: SetWorkerQuotaReason :execrows
UPDATE worker_quota_session SET reason = sqlc.arg(reason)
WHERE session_id = sqlc.arg(session_id) AND reason != sqlc.arg(reason);

-- name: ClaimWorkerQuotaAttempt :execrows
UPDATE worker_quota_session
SET attempt_owner = sqlc.arg(attempt_owner), target_harness = sqlc.arg(target_harness),
    target_model = sqlc.arg(target_model), reason = 'handoff started'
WHERE session_id = sqlc.arg(session_id) AND attempt_owner = '';

-- name: RecordWorkerQuotaFailure :exec
UPDATE worker_quota_session
SET failure_at = sqlc.arg(failure_at), reset_at = sqlc.arg(reset_at),
    failure_harness = sqlc.arg(failure_harness), failure_model = sqlc.arg(failure_model)
WHERE session_id = sqlc.arg(session_id) AND failure_at < sqlc.arg(failure_at);

-- name: ListWorkerQuotaFailures :many
SELECT failure_at, reset_at, failure_harness, failure_model FROM worker_quota_session
WHERE reset_at > sqlc.arg(now_ms);

-- name: DeferAutomationRunForQuota :exec
INSERT INTO worker_quota_automation_deferral (run_id, retry_at)
VALUES (sqlc.arg(run_id), sqlc.arg(retry_at))
ON CONFLICT(run_id) DO UPDATE SET retry_at = excluded.retry_at;
