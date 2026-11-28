-- name: GetPRConversationFeedbackState :one
SELECT pr_url, initialized_at
FROM pr_conversation_feedback_state
WHERE pr_url = ?;

-- name: UpsertPRConversationFeedbackState :exec
INSERT INTO pr_conversation_feedback_state (pr_url, initialized_at)
VALUES (?, ?)
ON CONFLICT (pr_url) DO NOTHING;

-- name: ListPRConversationFeedback :many
SELECT
    pr_url, comment_id, session_id, source_type, author, body, body_hash,
    url, state, auto_inject_review, created_at_provider, updated_at_provider, observed_at, unverified
FROM pr_conversation_feedback
WHERE pr_url = ?
ORDER BY COALESCE(created_at_provider, observed_at) ASC, comment_id ASC;

-- name: ListActivePRConversationFeedback :many
SELECT
    pr_url, comment_id, session_id, source_type, author, body, body_hash,
    url, state, auto_inject_review, created_at_provider, updated_at_provider, observed_at, unverified
FROM pr_conversation_feedback
WHERE pr_url = ? AND state = 'active' AND unverified = 0
ORDER BY COALESCE(created_at_provider, observed_at) ASC, comment_id ASC;

-- name: UpsertPRConversationFeedback :exec
INSERT INTO pr_conversation_feedback (
    pr_url, comment_id, session_id, source_type, author, body, body_hash,
    url, state, unverified, auto_inject_review, created_at_provider, updated_at_provider, observed_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (pr_url, comment_id) DO UPDATE SET
    session_id = excluded.session_id,
    source_type = excluded.source_type,
    author = excluded.author,
    body = excluded.body,
    body_hash = excluded.body_hash,
    url = excluded.url,
    state = excluded.state,
    unverified = excluded.unverified,
    auto_inject_review = excluded.auto_inject_review,
    created_at_provider = COALESCE(excluded.created_at_provider, pr_conversation_feedback.created_at_provider),
    updated_at_provider = COALESCE(excluded.updated_at_provider, pr_conversation_feedback.updated_at_provider),
    observed_at = excluded.observed_at;

-- name: MovePRAliasConversationFeedback :exec
INSERT OR IGNORE INTO pr_conversation_feedback (
    pr_url, comment_id, session_id, source_type, author, body, body_hash,
    url, state, unverified, auto_inject_review, created_at_provider, updated_at_provider, observed_at
)
SELECT
    sqlc.arg(canonical_url), src.comment_id, src.session_id, src.source_type, src.author, src.body, src.body_hash,
    src.url, src.state, src.unverified, src.auto_inject_review, src.created_at_provider, src.updated_at_provider, src.observed_at
FROM pr_conversation_feedback AS src
WHERE src.pr_url = sqlc.arg(previous_url);

-- name: DeletePRAliasConversationFeedback :exec
DELETE FROM pr_conversation_feedback WHERE pr_url = ?;

-- name: MovePRAliasConversationFeedbackState :exec
INSERT OR IGNORE INTO pr_conversation_feedback_state (pr_url, initialized_at)
SELECT sqlc.arg(canonical_url), src.initialized_at
FROM pr_conversation_feedback_state AS src
WHERE src.pr_url = sqlc.arg(previous_url);

-- name: DeletePRAliasConversationFeedbackState :exec
DELETE FROM pr_conversation_feedback_state WHERE pr_url = ?;
