-- +goose Up
-- Durable storage for explicit operator top-level PR conversation feedback
-- commands (`/ao review-feedback`), separate from view-only conversation
-- comments and formal review threads.
CREATE TABLE pr_conversation_feedback (
    pr_url TEXT NOT NULL REFERENCES pr(url) ON DELETE CASCADE,
    comment_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    source_type TEXT NOT NULL DEFAULT 'pr_conversation_comment',
    author TEXT NOT NULL,
    body TEXT NOT NULL,
    body_hash TEXT NOT NULL,
    url TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'baseline', 'deleted', 'superseded')),
    auto_inject_review INTEGER NOT NULL DEFAULT 1,
    created_at_provider DATETIME,
    updated_at_provider DATETIME,
    observed_at DATETIME NOT NULL,
    PRIMARY KEY (pr_url, comment_id)
);

CREATE INDEX idx_pr_conversation_feedback_pr_state
    ON pr_conversation_feedback(pr_url, state);

CREATE TABLE pr_conversation_feedback_state (
    pr_url TEXT PRIMARY KEY REFERENCES pr(url) ON DELETE CASCADE,
    initialized_at DATETIME NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS pr_conversation_feedback_state;
DROP INDEX IF EXISTS idx_pr_conversation_feedback_pr_state;
DROP TABLE IF EXISTS pr_conversation_feedback;
