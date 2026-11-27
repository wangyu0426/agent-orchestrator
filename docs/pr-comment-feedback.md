# Explicit Operator Review Feedback from PR Conversation Comments

By default, Agent Orchestrator treats top-level GitHub PR conversation comments as view-only metadata and routes only formal review summaries and inline review threads to workers.

When an operator posts feedback through a tool that only supports top-level PR comments (such as the ChatGPT GitHub connector), a project can opt in to ingesting **explicitly marked** operator feedback commands.

## Command Syntax

A top-level PR conversation comment is treated as actionable review feedback **only** when its exact first line starts with `/ao review-feedback` followed by non-empty actionable text (either on the same line or on subsequent lines):

```markdown
/ao review-feedback
Add a nil-check on the item parser and rerun the unit tests.
```

or inline:

```markdown
/ao review-feedback Add a nil-check on the item parser and rerun the unit tests.
```

Comments that do **not** match are ignored:
- Arbitrary prose or mentions without the exact first-line marker
- Empty marker lines with no actionable text
- Spoofed markers inside quotes (`> /ao review-feedback`), code fences, inline code, or later lines

## Project Configuration (Opt-In, Default Off)

Configure `prCommentFeedback` in the project's configuration:

```json
{
  "prCommentFeedback": {
    "enabled": true,
    "trustedAuthors": ["wangyu0426"],
    "enabledAt": "2026-11-27T17:00:00Z"
  }
}
```

- `enabled` (`bool`, default `false`): Must be `true` to enable polling and ingestion for the project.
- `trustedAuthors` (`[]string`): Explicit GitHub logins (matched case-insensitively) authorized to issue `/ao review-feedback` commands. Empty means no author is trusted.
- `enabledAt` (`string`, optional RFC3339 UTC timestamp): Establishes a baseline cutoff so comments created before `enabledAt` are recorded as historic baseline and never replayed on rollout.

Delivery also requires:
- The PR is open (not merged or closed) and currently claimed by a non-terminal session.
- The session has `autoInjectReview` enabled.

## Trust Policy and Shared-Account Caveat

- Provider-supplied author identity (`author.login` and `__typename`) is verified on every comment.
- Bots (including AO's own GitHub App), unknown/empty actors, and untrusted collaborators are always rejected, even if they include the `/ao review-feedback` marker.
- **Shared-account caveat**: A configured trusted operator may also be the PR author when a human connector and worker share a GitHub account. Only explicitly marked `/ao review-feedback` commands from that authorized identity are permitted; ordinary review-thread replies from the PR author remain filtered out. Because a shared GitHub account cannot cryptographically distinguish human versus agent provenance, **dedicated identities are required for stronger isolation**.
- Conversation feedback commands **never** become GitHub review verdicts, bypass same-head auto-review deduplication, grant merge permission, change worker credentials, or trigger automatic merges.

## Lifecycle, Baseline, Edit, Supersession, and Deletion

- **Baseline / No Historic Replay**: On the first observation after enablement, comments created before `max(enabledAt, pr.observed_at)` are recorded with `state = 'baseline'` and never delivered. Restarts preserve persisted dedup and retry state.
- **Edit**: Editing an active command updates its body hash and delivers the revised feedback. Editing a comment to remove the marker or empty the body marks it `deleted` and stops pending retries.
- **Supersession**: When a newer authorized `/ao review-feedback` command is posted on the same PR, older active commands are marked `superseded`.
- **Deletion**: Removing a comment on GitHub marks it `deleted` and stops pending retries.
- **Delivery & Recovery**: Active commands reuse the guarded delivery path (`sessionguard.Nudge`) and durable retry/backoff recovery (`pr.last_nudge_signature`), deferring while the worker is busy/input-gated and recovering across daemon restarts.

## Rollback

To immediately disable top-level PR conversation feedback for a project, set `prCommentFeedback.enabled` to `false` (or remove the `prCommentFeedback` block) in the project configuration. Polling for conversation comments stops on the next tick.
