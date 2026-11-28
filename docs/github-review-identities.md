# GitHub worker and reviewer identities

AO can use separate GitHub Apps for worker writes and formal automated reviews.
This is opt-in and builds on the repo-scoped worker credential broker. The
daemon's existing GitHub App is the default reviewer identity; a separate App
can be configured for workers. The human operator continues to use a personal
account for independent reviews and final merge authorization.

## Identity and permission matrix

| Role | Effective actor | Credential boundary | Repository permissions |
| --- | --- | --- | --- |
| Worker | Worker App bot, such as `yu-ao-worker[bot]` | The daemon's repo-scoped broker mints a short-lived, single-repository installation token. The token is provided to the worker's Git operations; the App key stays in the daemon. | Existing broker defaults: `contents:write`, `pull_requests:write`, `issues:write`, `metadata:read`, `checks:read`, `statuses:read`. No `workflows` or administrative permission. |
| Reviewer | Daemon App bot, such as `yu-ao-bot[bot]` | The reviewer submits structured AO results. The daemon mints its own short-lived token with `pull_requests:write` and `metadata:read`; no App token or private key enters the reviewer harness. | Pull request reviews and metadata for configured repositories. |
| Operator | A human GitHub account | Normal interactive GitHub login, kept separate from both App identities. `AO_GITHUB_APP_OPERATOR_LOGIN` is attribution metadata only; it does not authenticate or authorize provider actions. | Repository permissions assigned to the human by its owner. Use a human reviewer for the final merge gate. |

GitHub's create-review endpoint requires repository `Pull requests: write`.
The reviewer token is restricted to the one configured repository and those
permissions. The Worker App uses the existing #60 broker; this feature does not
add another token broker. GitHub documents the [review endpoint and permission](https://docs.github.com/en/rest/pulls/reviews#create-a-review-for-a-pull-request)
and the [line and side fields for inline comments](https://docs.github.com/en/rest/pulls/comments#create-a-review-comment-for-a-pull-request).

## Configuration

First configure the existing daemon App and repo-scoped worker broker. Keep both
private keys readable only by the daemon account. The repositories are
repository names within `AO_GITHUB_APP_INSTALLATION_OWNER`.

```sh
AO_GITHUB_APP_ENABLED=on
AO_GITHUB_APP_ID=12345
AO_GITHUB_APP_INSTALLATION_ID=67890
AO_GITHUB_APP_PRIVATE_KEY_PATH=/secure/ao/reviewer-app.pem
AO_GITHUB_APP_INSTALLATION_OWNER=your-org
AO_GITHUB_APP_REPOSITORIES=your-repo

AO_GITHUB_APP_WORKER_BROKER_ENABLED=on
AO_GITHUB_APP_WORKER_APP_ENABLED=on
AO_GITHUB_APP_WORKER_APP_ID=54321
AO_GITHUB_APP_WORKER_INSTALLATION_ID=98765
AO_GITHUB_APP_WORKER_PRIVATE_KEY_PATH=/secure/ao/worker-app.pem
```

`AO_GITHUB_APP_WORKER_API_BASE_URL` is optional and defaults to
`AO_GITHUB_APP_API_BASE_URL` (or `https://api.github.com`). If supplied, it
must match the daemon App API base. The Worker App must be installed on every
repository the broker can issue tokens for. Its installation must grant the
worker permissions listed above. The daemon App installation must grant
`Pull requests: write` and repository metadata access for those repositories.

AO rejects equal configured App IDs. On a submission it also resolves the
actual bot logins from GitHub's App identity endpoint and compares actors. A
different installation of the same App is still the same actor and is
rejected. The reviewer actor is also compared to both the recorded and live PR
author. When GitHub forbids a native review or actors collide, AO records the
local verdict with provider state `DEGRADED` and a safe reason; it does not
retry as the operator or as the worker. A verified `COMMENT` result is recorded
as `COMMENTED`, which does not mean GitHub approved or rejected the PR.

For `COMMENT` events, AO's structured review-channel parser can route an
internal `changes_requested` or `approved` verdict to the worker while keeping
GitHub's formal decision unchanged. Enable that existing channel and explicitly
trust the reviewer App login:

```sh
AO_STRUCTURED_REVIEW_ENABLED=on
AO_STRUCTURED_REVIEW_TRUSTED_REVIEWERS=yu-ao-bot[bot]
```

Native `REQUEST_CHANGES` and `APPROVE` reviews use GitHub's review state and do
not depend on the structured-body allowlist. Structured review feedback is
read only from pull request review records. Ordinary bot issue/status comments
do not become trusted structured review events.

## Review submission and recovery

Reviewers send AO a result containing the AO verdict, a provider event, summary
body, and optional inline findings. AO binds it to the stored review run, exact
repository and PR, and the full head SHA captured when that pass started. It
checks the current PR head before posting and verifies the returned review's
actor, state, ID, and commit SHA. A mismatch is never reported as native
success.

AO stores its verdict separately from the provider observation. Provider states
include `APPROVED`, `CHANGES_REQUESTED`, `COMMENTED`, `DEGRADED`,
`LEGACY_UNVERIFIED`, and `NOT_POSTED`. Retried submissions carry a stable
run marker. AO scans the PR review history before posting and reconciles an
ambiguous response; it reuses a matching review, rejects a contradictory retry,
and leaves an inconclusive delivery retryable instead of claiming success.
Concurrent submissions for one run are serialized in the daemon.

When a verified native `REQUEST_CHANGES` review is observed, the existing SCM
feedback path sends actionable review feedback to the owning worker once, using
its durable delivery and retry state. A bot author is not filtered out of the
formal review channel. Conversation/status comments continue through their own
authorization and feedback paths. A `COMMENT` result needs the structured
review policy above if its AO verdict should affect worker feedback.

## Actions and branch rules

Worker App pushes and pull request creation are attributed to the Worker App.
That actor appears in audit records and is available to workflow conditions.
GitHub documents that events from `GITHUB_TOKEN` have workflow-trigger
exceptions, while a GitHub App installation token can trigger the corresponding
events; see [workflow trigger behavior](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/trigger-a-workflow).
Check workflows that filter on `github.actor`, `author_association`, or a
particular human login. The worker App does not receive `workflows:write`; a
worker change that edits workflow files may therefore require a separate human
path. Keep `pull_request_target` workflows especially constrained because
they run in the base-branch context.

Native App reviews are visible in GitHub and can affect repository rules. A
`REQUEST_CHANGES` review from a collaborator with sufficient repository access
can block merging until that reviewer approves or a permitted user dismisses
the review. GitHub also prevents a PR author from approving their own PR. Bot
approvals may be displayed but are not guaranteed to satisfy a required-review
rule; behavior depends on the repository's rule and reviewer permissions. Read
GitHub's guidance on [required reviews](https://docs.github.com/en/pull-requests/how-tos/review-pull-requests/approving-a-pull-request-with-required-reviews)
and [protected branches](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches).

Before enabling on a repository, an operator should verify its actual branch
protection or ruleset with separate worker and reviewer actors. Keep the human
approval requirement explicit (for example, require an eligible human or code
owner review) and retain the normal human merge authorization. AO neither edits
branch rules nor merges automatically. If the repository denies the reviewer
App or excludes bot reviews, AO surfaces `DEGRADED`; do not weaken its rules to
make AO report success.

## Migration and rollback

1. Create and install the Worker App with the broker permissions above. Give it
   access only to the repositories AO should manage.
2. Keep the existing daemon App installed with reviewer permissions. Verify the
   two Apps resolve to different GitHub bot logins.
3. Set the Worker App variables and enable the broker. Start with a test
   repository and confirm the PR author is the Worker App and a review is
   submitted by the daemon App on the exact reviewed head.
4. Verify the worker receives one changes-requested review, a subsequent review
   can approve the new head, Actions run under expected policy, and the final
   human review/merge gate remains in force.

Existing PRs are not rewritten or reassigned. If an existing PR is authored by
the reviewer App actor, that actor cannot submit a formal review on it; AO keeps
the local verdict and marks provider delivery degraded. Continue it with an
independent human review or open a new Worker-App-authored PR when the operator
chooses. AO does not copy branches or create replacement PRs automatically.

To roll back the separate route, stop starting new automated review passes,
disable `AO_GITHUB_APP_WORKER_APP_ENABLED`, and then disable the worker broker
when intentionally returning to its previous credential mode. Native review
submission requires both the broker and the distinct Worker App; without both,
AO uses the existing legacy reviewer path. Do not silently switch to operator
credentials. Keep the additive database migration in place during application rollback: the new
provider-state columns are harmless when unused and preserve review history.
The migration's down step drops those observations and should be used only if
the database itself is being restored to a pre-migration backup/version.

Legacy mode remains supported. Existing legacy GitHub CLI review submissions
are marked `LEGACY_UNVERIFIED`; AO cannot infer a verified native provider state
from an old review ID. Existing PR authorship and human merge policy are left
unchanged.

## Live verification status

The automated tests use local HTTP fakes and never write to GitHub. The live
`ao-eval-lab` smoke must be run by an operator with both App installations and
must inspect the resulting GitHub review and branch-rule behavior. It is not a
CI step and this implementation does not perform live repository writes.
