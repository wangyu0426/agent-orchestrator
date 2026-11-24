# Worker quota failover

Worker failover is a daemon-wide opt-in. It applies to tracker intake, scheduled
workers, and workers spawned by another AO session. Each project's existing
worker harness, model, and reasoning effort are the default primary. AO checks
routes in configuration order and skips a route when a current applicable
quota window is exhausted.

## Precedence

1. **Explicit spawn overrides** — manual task creation, or any explicit spawn
   harness, account, model, interface (`--mode`), native mode, or reasoning
   effort — take precedence and bypass worker quota routing.
2. **Selected worker quota route** (`AO_WORKER_QUOTA_PRIMARY` if set, otherwise
   the project's worker settings as primary, followed by
   `AO_WORKER_QUOTA_FALLBACKS` in order): the selected route's `harness`,
   `model`, `effort`, and optional `mode` override project defaults and the
   daemon's default session interface.
3. **Daemon and project defaults** — when a route omits `mode`, AO resolves the
   session interface from the daemon default (falling back to TUI if default
   Chat is unavailable for that harness, or Chat if the harness is Chat-only).

Interactive task creation, orchestrator sessions, and reviewer routing continue
to use their own settings.

## Configuration

Set the following environment variables for the AO daemon and restart it.
The optional primary takes precedence over every project's worker settings.
A target with an omitted model or effort uses the target provider's default.
An optional `"mode"` (`"chat"` or `"tui"`) pins the session interface for new
workers admitted on that route; omitting `"mode"` preserves default behavior.
Invalid mode values and incompatible harness/mode combinations (such as `"tui"`
on a Chat-only harness or `"chat"` on a harness without a Chat driver) are
rejected at configuration load.
Use the model IDs accepted by the chosen harness.

```sh
export AO_WORKER_QUOTA_FAILOVER=on
export AO_WORKER_QUOTA_FALLBACKS='[{"harness":"agy","model":"your-agy-model-id","mode":"tui"}]'
# Optional AO-wide primary override
export AO_WORKER_QUOTA_PRIMARY='{"harness":"codex","model":"your-codex-model-id","effort":"high"}'
```

### Rollout example (Codex primary with AGY TUI fallback)

To keep Codex as the primary worker harness and fall back to AGY Hydrogen Low
in Terminal UI mode when Codex weekly quota is exhausted:

```sh
export AO_WORKER_QUOTA_FAILOVER=on
export AO_WORKER_QUOTA_FALLBACKS='[{"harness":"agy","model":"gemini-4-hydrogen-low","mode":"tui"}]'
export AO_QUOTUM_URL='https://quotum.example.com'
export AO_QUOTUM_BOARD='board-id'
export AO_QUOTUM_SESSION_FILE='/path/to/private/quotum-session'
export AO_QUOTUM_BINDINGS='{
  "codex":{"sourceId":"codex-source-id"},
  "agy":{"sourceId":"agy-source-id","modelGroups":{"gemini-4-hydrogen-low":["gemini:session","gemini:weekly"]}}
}'
```

When `"mode":"tui"` is configured, AO admits the route using TUI prerequisites
(installed binary and terminal runtime) without requiring AGY ACP Chat
authentication, and launches the worker directly in TUI. Optional AI task naming
is best-effort and falls back to a deterministic prompt slug if background Chat
naming is unavailable.

AO can run without Quotum. A confirmed Codex Chat refusal with a known
rolling reset can defer new work for the same model on the computer login.
For an omitted Codex model, AO reads the configured default from an ephemeral
read-only thread and pins that concrete model for the launch. If discovery
cannot identify the model, native evidence for a specific model leaves that
route eligible. With no confirmed quota evidence a route remains eligible. A missing reading,
stale snapshot, expired window, authentication error, transport error, or
transient throttle is treated as unknown quota. If none of the configured routes can launch, AO
reports that no route is ready. An AO-wide primary override replaces the
project default in the admission policy.

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
  handoff note and a continuation prompt. AO saves the order of remaining routes
  at admission, so a worker that started on a fallback can only move forward
  in its original order. Changes to route order apply to new workers.
  One durable attempt per session prevents repeated switching, including after
  daemon restart. If interrupted after
  switching, inspect the active provider and send a continuation in that session.
  Remaining fallback routes that pin `"mode":"tui"` are not reinterpreted as Chat
  for an in-flight Chat session; if no Chat-compatible fallback remains, the
  session parks safely for manual recovery.
- **Parking.** An unsupported or unavailable fallback leaves the failed turn
  available for manual recovery. AO stops the provider when existing idle and
  ownership checks permit it. Terminal UI workers with a confirmed Codex
  transcript failure use this path because automatic terminal replacement
  requires conclusive rollback of a failed target launch. Resume the existing
  session or use its provider switch controls. No separate worktree is created.
  Pending Chat input, queued work, and user-armed usage-limit auto-resume take
  precedence over automatic switching.

## Diagnosing and recovering paused launches

Repeated identical configuration or authentication failures (3 consecutive
failures) pause only the failing effective route configuration `(project,
harness, model, effort, mode)` rather than poisoning the configured primary or
unrelated routes. Healthy alternative routes remain eligible according to policy.

To inspect active pauses without modifying credentials or sessions:

- Check daemon logs for `worker quota selected configured route` and
  `session: automatic launch configuration paused` (both record the effective
  `harness`, `mode`, and `code`).
- Query `GET /api/v1/projects` and inspect each project's `autoSpawnPauses`
  list (showing `harness`, `model`, `effort`, `mode`, `errorCode`, `message`,
  `failures`, and `pausedAt`).

To recover from a pause without deleting credentials or sessions:

- **Restart the daemon** after fixing environment configuration (for example,
  switching a fallback route to `"mode":"tui"`). Pause streaks are held in
  daemon memory and clear on restart while preserving all sessions and
  credentials.
- **Run one manual session** matching the paused harness, model, effort, and
  interface; a successful manual start clears the matching pause immediately.
- **Update the project's agent/worker settings**, which invalidates the cached
  pause fingerprint for that project.
- **Wait for the 30-minute cooldown**, after which the daemon automatically
  admits one probe attempt for that configuration.

Selected non-primary routes, deferred intake, and handoff or parking reasons are
recorded in daemon logs. The session shows its active harness and model. Review
the failed turn and daemon logs using the session ID when manual recovery is
needed. Codex is the initial source of native quota refusals. Quotum can route
new workers around an exhausted AGY pool, but AO needs an authoritative native
AGY failure signal before an AGY turn can trigger automatic handoff. Live Codex
and AGY quota exhaustion and a production Quotum instance require separate live
verification. The automated suite uses deterministic quota windows and simulated
provider failures.
