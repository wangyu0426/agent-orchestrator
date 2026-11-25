# Structured PR Review-Body Decisions

Agent Orchestrator (AO) supports an opt-in, deterministic protocol for recognizing structured review decisions inside submitted GitHub `COMMENTED` pull request review bodies. This enables single-account reviewer workflows (where GitHub disallows formal self-approval and self-request-changes) without requiring a second GitHub account or GitHub App, and without interpreting free-text prose with an LLM.

## Protocol

A submitted `COMMENTED` review is recognized as a structured decision **only** when its body starts at line 1 with a strict two-line header:

```text
AO-REVIEW: REQUEST_CHANGES
HEAD: <full 40- or 64-character hex commit SHA>

[P2] Actionable finding description...
```

or:

```text
AO-REVIEW: APPROVE
HEAD: <full 40- or 64-character hex commit SHA>
```

### Strict Validation Rules

- **Position:** The header must appear at the very beginning of a submitted review body. Quoted markers (`> AO-REVIEW:`), prose before the header, PR descriptions, issue comments, pending reviews, and dismissed reviews are ignored.
- **Commit binding:** `HEAD` must be a full commit SHA and must match the review's GitHub `commit.oid` (`TargetSHA`).
- **Non-empty change requests:** `AO-REVIEW: REQUEST_CHANGES` requires non-empty actionable feedback after the header.
- **HEAD-bound approval:** `AO-REVIEW: APPROVE` is valid only while `HEAD` matches the PR's current `HeadSHA`. Pushing a new commit immediately invalidates an earlier structured approval.

## Reviewer Trust Configuration

Structured review-body decisions are **disabled by default** to preserve full compatibility with existing repositories and ordinary review comments.

Configure them via daemon environment variables:

| Environment Variable | Default | Description |
| --- | --- | --- |
| `AO_STRUCTURED_REVIEW_ENABLED` | `false` | Master opt-in switch (`true`/`1` to enable). |
| `AO_STRUCTURED_REVIEW_TRUSTED_REVIEWERS` | empty | Comma-separated list of authorized GitHub logins (case-insensitive; leading `@` is ignored). |
| `AO_STRUCTURED_REVIEW_ALLOW_PR_AUTHOR` | `false` | Explicitly authorizes the PR author's own GitHub account to emit structured `AO-REVIEW:` decisions on their own PRs for single-account workflows. |

### Trust Boundary & Limitations

- When `AO_STRUCTURED_REVIEW_ENABLED=true`, AO fails closed: a reviewer is authorized **only** if their login is in `AO_STRUCTURED_REVIEW_TRUSTED_REVIEWERS` or `AO_STRUCTURED_REVIEW_ALLOW_PR_AUTHOR=true` and they authored the PR.
- Ordinary prose comments from an authorized reviewer (including the PR author) are **never** treated as decisions.
- Single-account setups share one GitHub identity between the PR author and the reviewer; AO relies on the strict `AO-REVIEW:` header rather than separate GitHub credentials. Structured decisions do **not** bypass GitHub branch protection, required reviewers (`REVIEW_REQUIRED`), or unresolved inline threads, and do **not** enable auto-merge.

## Ordering, Supersession, and Native Reviews

- Native GitHub `APPROVED` and `CHANGES_REQUESTED` reviews are preserved and evaluated alongside structured decisions per reviewer, ordered by `SubmittedAt` (tie-broken by review ID).
- An informal `COMMENTED` review never clears an earlier `REQUEST_CHANGES` decision.
- Pushing a new commit alone does **not** clear a structured `REQUEST_CHANGES`; an explicit eligible re-review/decision (structured `APPROVE` matching the current `HEAD`, or a native GitHub review) is required. An ineligible or stale `AO-REVIEW: APPROVE` targeted at an older commit never clears an active `REQUEST_CHANGES`.
- Any active `CHANGES_REQUESTED` (native or structured) takes precedence over `APPROVED` from another reviewer.
- A structured `APPROVE` never overrides native `CHANGES_REQUESTED`, native `REVIEW_REQUIRED`, or unresolved actionable inline threads.

## Worker Delivery, Idempotency, and Diagnostics

- Active structured `REQUEST_CHANGES` reviews are delivered to the existing worker session through AO's durable reaction path (`sendOnce`), preserving the worker session/worktree and avoiding duplicate workers.
- Delivery is keyed by `structured-review:<prURL>:<reviewID>` with a deterministic signature over the decision, reviewed SHA, and feedback body, persisted in SQLite (`pr.last_nudge_signature`). Failed deliveries are not marked delivered and retry on subsequent polls; successful deliveries survive concurrent polls and daemon restarts without duplicate dispatch.
- `structuredreview.BuildDiagnostics` keeps `githubDecision` separate from AO's local `effectiveDecision`, exposing `source` (`native` vs `structured`), `reviewedSha`, and `pendingFeedback` items with `deliveryStatus` (`pending` vs `delivered`, distinct from addressed).

## Compatibility & Manual Recovery

- Historical unstructured reviews (such as free-text findings in #21) are not inferred as machine decisions and remain visible for manual triage.
- To recover manually from a stale or mistaken structured `REQUEST_CHANGES`, an authorized reviewer can submit a new structured `AO-REVIEW: APPROVE` on the current `HEAD` (or a native GitHub review) to supersede the prior decision.
