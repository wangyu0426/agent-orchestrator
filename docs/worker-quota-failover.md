# Worker quota failover

Worker failover is a daemon-wide opt-in. It applies to tracker intake, scheduled
workers, and workers spawned by another AO session. Each project's existing
worker harness, model, and reasoning effort are the default primary. AO checks
routes in configuration order and skips a route when a current applicable
quota window is exhausted. An explicit spawn harness, account, model, native
mode, or reasoning effort takes precedence. Interactive task creation,
orchestrator sessions, and reviewer routing continue to use their own settings.

## Configuration

Set the following environment variables for the AO daemon and restart it.
The optional primary takes precedence over every project's worker settings.
A target with an omitted model or effort uses the target provider's default.
Use the model IDs accepted by the chosen harness.

```sh
export AO_WORKER_QUOTA_FAILOVER=on
export AO_WORKER_QUOTA_FALLBACKS='[{"harness":"agy","model":"your-agy-model-id"}]'
# Optional AO-wide primary override
export AO_WORKER_QUOTA_PRIMARY='{"harness":"codex","model":"your-codex-model-id","effort":"high"}'
```

AO can run without Quotum. A confirmed Codex Chat refusal with a known
rolling reset can defer new work for the same model on the computer login.
With no confirmed quota evidence a route remains eligible. A missing reading,
stale snapshot, expired window, authentication error, transport error, or
transient throttle is treated as unknown quota.

### Optional Quotum hub

Use the [self-hosted Quotum hub](https://github.com/padurets/Quotum), with a
board that can see the subscriptions AO runs under. AO reads the hub's
[`GET /api/overview?board=…` dashboard projection](https://github.com/padurets/Quotum/blob/7b9c8af7fc811b5859b39c3f3c81da9269ca2895/spec/dashboard-v1.md).
AO sends the existing `quotum_session` cookie over HTTPS or loopback HTTP.
It reads cached observations and issues no refresh, login, or mutation requests.

```sh
export AO_QUOTUM_URL='https://quotum.example.com'
export AO_QUOTUM_BOARD='board-id'
export AO_QUOTUM_SESSION_FILE='/path/to/private/quotum-session'
export AO_QUOTUM_BINDINGS='{
  "codex":{"sourceId":"codex-source-id"},
  "agy":{"sourceId":"agy-source-id","modelGroups":{"your-agy-model-id":["gemini:session","gemini:weekly"]}}
}'
```

Store only the cookie value in the session file and keep the file private to the
daemon user. Renew the file and restart the daemon when the hub session expires.
The board selection defaults to the hub's first board for that account.

Copy each source ID and the relevant window IDs from your board's overview.
Map each harness to the board source for the same computer login AO uses.
AO ignores those bindings for launches using a different saved account.
Unscoped windows apply to every model on that source. For model groups such as
Antigravity's Gemini and third-party pools, list each applicable window ID for
each configured model. Use an empty model key to bind scoped windows for a
provider-default model. Models that share a pool should reference the same window
IDs. AO treats fresh 100% readings as exhausted until the reported reset or the
snapshot loses freshness, whichever comes first. Board source IDs and window
schema are tied to the Quotum hub release.

## Exhaustion and recovery

- **New workers.** The first eligible route is selected at spawn. On exhaustion
  AO returns `WORKER_QUOTA_DEFERRED` before creating a session or worktree.
  Tracker intake retries on its existing five-minute backoff. Scheduled runs
  retain their original occurrence and retry after one minute. A recovered
  primary takes precedence on the next spawn. A healthy fallback keeps running.
- **Chat workers.** AO recognizes Codex's native `usageLimitExceeded` failure.
  For a settled turn it can use an eligible cross-provider Chat driver, including
  AGY on installations with its Chat driver available and a compatible approval
  mode. The switch retains the AO session, worktree, issue and PR associations,
  and canonical history. The target starts a new native conversation with the
  handoff note and a continuation prompt. One durable attempt per session prevents
  repeated switching, including after daemon restart. If interrupted after
  switching, inspect the active provider and send a continuation in that session.
- **Parking.** An unsupported or unavailable fallback leaves the failed turn
  available for manual recovery. AO stops the provider when existing idle and
  ownership checks permit it. Terminal UI workers with a confirmed Codex
  transcript failure use this path because automatic terminal replacement
  requires conclusive rollback of a failed target launch. Resume the existing
  session or use its provider switch controls. No separate worktree is created.
  Pending Chat input, queued work, and user-armed usage-limit auto-resume take
  precedence over automatic switching.

Selected non-primary routes, deferred intake, and handoff or parking reasons are
recorded in daemon logs. The session shows its active harness and model. Review
the failed turn and daemon logs using the session ID when manual recovery is
needed. Codex is the initial source of native quota refusals. Quotum can route
new workers around an exhausted AGY pool, but AO needs an authoritative native
AGY failure signal before an AGY turn can trigger automatic handoff. Live Codex
and AGY quota exhaustion and a production Quotum instance require separate live
verification. The automated suite uses deterministic quota windows and simulated
provider failures.
