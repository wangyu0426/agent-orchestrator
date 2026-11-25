-- +goose Up

CREATE TABLE reviewer_quota_deferral (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    pr_url TEXT NOT NULL,
    target_sha TEXT NOT NULL,
    retry_at INTEGER NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, pr_url, target_sha)
);

-- Reclassify historical pre-upgrade failed review runs whose correlated
-- reviewer Chat turn failed with typed failure_code = 'quota_exhausted' so
-- pending PR heads are not permanently stranded by the ordinary retry cap.
UPDATE review_run
SET body = 'reviewer Chat turn failed: quota_exhausted'
WHERE status = 'failed'
  AND verdict = ''
  AND batch_id != ''
  AND body = 'reviewer Chat turn ended without submitting a result'
  AND EXISTS (
    SELECT 1 FROM conversation_turns AS turn
    JOIN conversation_messages AS message
      ON message.turn_id = turn.id
     AND message.conversation_id = turn.conversation_id
    WHERE turn.handled_by_review_id = review_run.review_id
      AND turn.failure_code = 'quota_exhausted'
      AND message.role = 'user'
      AND message.origin = 'daemon'
      AND message.client_message_id = 'review-batch:' || review_run.batch_id
  );

-- +goose Down

DROP TABLE reviewer_quota_deferral;
