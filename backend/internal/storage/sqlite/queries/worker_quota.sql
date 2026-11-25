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
WHERE session_id = sqlc.arg(session_id) AND attempt_owner = ''
  AND EXISTS (SELECT 1 FROM sessions WHERE sessions.id = sqlc.arg(session_id) AND sessions.model = sqlc.arg(expected_model));

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

-- name: ListReviewerQuotaFailures :many
SELECT
    review.harness AS failure_harness,
    turn.completed_at AS failure_at,
    turn.usage_limit_resets_at AS reset_at
FROM conversation_turns AS turn
JOIN review ON review.id = turn.handled_by_review_id
WHERE turn.handled_by_review_id IS NOT NULL
  AND turn.completed_at IS NOT NULL
  AND (turn.failure_code = 'quota_exhausted' OR turn.usage_limit_resets_at IS NOT NULL);

-- name: UpsertReviewerQuotaDeferral :exec
INSERT INTO reviewer_quota_deferral (session_id, pr_url, target_sha, retry_at, reason, updated_at)
VALUES (sqlc.arg(session_id), sqlc.arg(pr_url), sqlc.arg(target_sha), sqlc.arg(retry_at), sqlc.arg(reason), sqlc.arg(updated_at))
ON CONFLICT(session_id, pr_url, target_sha) DO UPDATE SET
    retry_at = excluded.retry_at,
    reason = excluded.reason,
    updated_at = excluded.updated_at;

-- name: GetReviewerQuotaDeferral :one
SELECT retry_at, reason
FROM reviewer_quota_deferral
WHERE session_id = sqlc.arg(session_id)
  AND pr_url = sqlc.arg(pr_url)
  AND target_sha = sqlc.arg(target_sha);

-- name: DeleteReviewerQuotaDeferral :exec
DELETE FROM reviewer_quota_deferral
WHERE session_id = sqlc.arg(session_id)
  AND pr_url = sqlc.arg(pr_url)
  AND target_sha = sqlc.arg(target_sha);
