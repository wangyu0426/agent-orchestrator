# Experimental Codex Native Review loopback permission profile

This opt-in experiments with restoring the **existing** Reviewer → AO CLI →
loopback HTTP → daemon result-submission path. It does **not** deploy PR #2's
filesystem inbox, add API auth, modify the daemon's listener, or allow merge.

## Why it is opt-in

AO's Codex TUI reviewer normally receives `--sandbox read-only`. Codex's
permission profiles cannot be combined with that legacy flag. With the host
environment variable `AO_CODEX_REVIEW_LOOPBACK_PROFILE=1`, the reviewer TUI
launch **and resume** instead receive
`-c 'default_permissions="ao-review-loopback"'`. All other sessions and
reviewer harnesses keep their original policies. The named profile must exist
in the **effective Codex user configuration on the execution host**.

Unset the environment variable (and relaunch the reviewer) to roll back. A
missing or malformed profile should fail the reviewer launch; do not silently
fall back to broader sandbox permissions.

## Host Codex configuration (example)

Use the actual Codex configuration home for the AO daemon's user (often
`~/.codex/config.toml`). Merge this with the existing configuration rather
than overwriting authentication or other settings:

```toml
[features]
network_proxy = true

[permissions.ao-review-loopback]
extends = ":read-only"

[permissions.ao-review-loopback.network]
enabled = true
allow_local_binding = true

[permissions.ao-review-loopback.network.domains]
"127.0.0.1" = "allow"
"localhost" = "allow"
# If the reviewer itself posts a GitHub review, also allow its API endpoint:
# "api.github.com" = "allow"
```

**Read the effective configuration first.** A legacy `sandbox_mode`,
`sandbox_workspace_write`, or an explicit `--sandbox` in another active
config layer selects the legacy sandbox path instead of permission profiles.
Remove conflicting *legacy sandbox keys from the profile in use* only after
inspecting the impact on other Codex sessions; do not blindly change a
shared global policy.

The network proxy must actually be active for its domain allowlist to be
enforced. `allow_local_binding=true` intentionally broadens access to local
network destinations; the allowlist is **not a TCP port 3301 firewall**.
Go's standard HTTP client bypasses proxy settings for loopback IPs, and
can therefore be blocked by the OS sandbox even when proxied `curl` works.
For Go `ao`, validate **direct** loopback as well as proxy-mediated HTTP.
On this single-user host, `allow_local_binding = true` is the proposed
profile adjustment if the direct TCP canary is blocked. It broadens private
network access; this isn't a per-port permission. Never equate `curl`
success alone with successful AO CLI → daemon communication.
A GitHub review POST from the reviewer also needs network access to
`api.github.com` and a working authorized `gh` login; if only loopback is
allowed, validate AO result transport separately and report GitHub posting
as blocked until that dependency is available.
Neither filesystem read-only nor the network allowlist hides host credentials
that the sandbox can already read.

## Verification and rollout (no production changes until proven)

1. **Run diagnostics from a real host SSH/login shell, NOT a shell tool
   running inside an existing Codex session.** First record the Codex CLI
   version and inspect the effective config without printing tokens.
   Check the host's scratch daemon responds, then run the read-only-profile
   canary directly from that same host shell:

   ```bash
   codex --version
   curl -fsS --max-time 3 http://127.0.0.1:<scratch-port>/healthz
   codex sandbox -P ao-review-loopback -- \
     /bin/true
   codex sandbox -P ao-review-loopback -- \
     curl -fsS --max-time 3 http://127.0.0.1:<scratch-port>/healthz
   ```

   **Known upstream pitfall:** Codex 0.162.1 can fail on nested Linux
   sandbox initialization before executing `/bin/true` or `curl`:
   `app-server socket directory must be a user-owned directory with
   mode 0700`. The outer Codex sandbox masks
   `/tmp/codex-daemon-<UID>` with a mode-000 tmpfs, whereas the nested
   sandbox requires 0700. See
   [openai/codex#53799](https://github.com/openai/codex/issues/53799).
   A direct host invocation can avoid this *nested* failure; it is not a
   guarantee that the loopback network path is working. If a direct host
   invocation also fails, inspect `stat -c '%u %a %n' /tmp/codex-daemon-$(id -u)`
   and `findmnt -T /tmp/codex-daemon-$(id -u)` from the host shell.
   Do not blindly chmod/chown/delete Codex's shared socket directory.
   A read-only PATH-alias warning is separate from the bubblewrap error.

2. Test a **scratch** AO instance and an unmerged disposable PR. The AO daemon
   must inherit `AO_CODEX_REVIEW_LOOPBACK_PROFILE=1`. Launch a **new** Codex
   reviewer TUI through real AO, not only `codex sandbox linux` or manual
   `codex exec`. Confirm the argv has the named profile and *no*
   `--sandbox`, `--add-dir`, or full-access flag.
3. In that same reviewer, use `ao` to reach the scratch daemon via its
   actual run-file / port override. Test `/healthz`, then submit one
   real review through the existing `ao review submit` contract.
4. Confirm the exact PR, expected latest HEAD SHA, GitHub review ID, persisted
   run verdict, duplicate-submission behaviour and reviewer-exit/restart
   recovery. Check the reviewer cannot **write** its checkout, other AO state,
   or host files. Do not read secret values during canaries.
5. Stop/restart only the disposable instance and clean it up. Do not change
   production settings or enable autoReview until the full scratch E2E passes.

Any `EPERM`, connection refusal, wrong daemon, unexpected permissions,
review ID mismatch, or missing durable result means **BLOCKED**. The Codex
proxy/OS namespace may still prevent host loopback access even with these
settings; do not call this feature working based on syntax-only or unit tests.

## Sandbox-visible HTTP but PID-invisible: stale run-file fix

A real SSH test on 2026-11-11 confirmed:

- Host `/tmp/codex-daemon-1000` is owner UID 1000 with mode 0700.
- Both `codex sandbox -P ao-review-loopback -- /bin/true` and the
  sandboxed `curl http://127.0.0.1:3301/healthz` succeeded.
- The **old** sandboxed `ao review ls path-of-reasoning-4 --json`
  reported a stale run-file despite that HTTP health result.

The CLI used `kill(pid, 0)` before contacting the daemon. A sandbox's
PID namespace can hide the host PID, causing a false stale-run-file result.
This PR now verifies `/healthz` on the run-file's loopback port **only when**
the PID probe fails. It accepts the daemon only if both the service name and
the exact run-file PID match. Wrong/missing/unhealthy endpoints remain stale.

To test **without replacing the running AO binary or restarting the daemon**,
use a host SSH shell and the existing PR test worktree. **Never paste
`set -e` into an interactive SSH shell**: a failing test can exit the shell
and disconnect SSH. The chain below stops before build on a test failure,
yet returns control to the prompt.

```bash
set +e
(
  cd "$HOME/ao-eval-lab/ao-pr4-check" &&
  git fetch origin pull/4/head &&
  git switch --detach FETCH_HEAD &&
  cd backend &&
  go test ./internal/cli ./internal/adapters/reviewer/codex &&
  go build -o "$HOME/ao-eval-lab/ao-pr4-cli" ./cmd/ao &&
  codex sandbox -P ao-review-loopback -- \
    env AO_RUN_FILE="$HOME/.ao-orchestrator-test/running.json" \
        AO_DATA_DIR="$HOME/.ao-orchestrator-test/data" \
        "$HOME/ao-eval-lab/ao-pr4-cli" review ls path-of-reasoning-4 --json
)
echo "Verification exit code: $?"
```

If the `ao-pr4-check` worktree does not already exist, create it once
from the host shell with `git -C ~/orchestrator-eval fetch origin
pull/4/head` followed by `git -C ~/orchestrator-eval worktree add
--detach ~/ao-eval-lab/ao-pr4-check FETCH_HEAD`.

An empty review list with no transport error is still a successful CLI
connection. The old live daemon remains untouched by this procedure.
This is **not** yet proof of a real AO-launched Reviewer or review submission.
`go test` must pass and the clean-worktree build must use the current PR
HEAD before declaring this test passed.

## Direct vs proxied loopback diagnostic

If the patched CLI's `review ls` still reports a stale run-file despite
two passing Go test packages and a successful sandbox `curl`, distinguish
run-file identity mismatch from **Go direct-socket denial**:

```bash
set +e
RUN="$HOME/.ao-orchestrator-test/running.json"
python3 -c 'import json,sys; j=json.load(open(sys.argv[1])); print("runfile PID/port:",j["pid"],j["port"])' "$RUN"
codex sandbox -P ao-review-loopback -- \
  curl --noproxy '*' -fsS --max-time 3 http://127.0.0.1:3301/healthz
codex sandbox -P ao-review-loopback -- \
  curl -fsS --max-time 3 http://127.0.0.1:3301/healthz
```

If PID/port match the health JSON and **direct** `--noproxy '*'` fails
while ordinary curl succeeds, try the permission change **for this one
canary only** without modifying the global Codex config:

```bash
codex sandbox -P ao-review-loopback \
  -c 'permissions.ao-review-loopback.network.allow_local_binding=true' -- \
  env AO_RUN_FILE="$HOME/.ao-orchestrator-test/running.json" \
      AO_DATA_DIR="$HOME/.ao-orchestrator-test/data" \
      "$HOME/ao-eval-lab/ao-pr4-cli" review ls path-of-reasoning-4 --json
```

If the override succeeds, add `allow_local_binding = true` under
`[permissions.ao-review-loopback.network]` to persist it. If it fails,
do not guess: compare the run-file PID/port against the direct HTTP response
and collect a precise Go socket denial with a diagnostic build. Never
restart the healthy daemon to work around a client-side check.

## Codex HTTP proxy for Go-based AO CLI (opt-in)

On the real `yuworker` machine, the host CLI successfully listed PoR #33's
`needs_review` state, and ordinary sandbox `curl` could reach
`127.0.0.1:3301`. But `curl --noproxy '*'` failed, and the Go CLI was still
blocked even with `allow_local_binding=true`. The problem is a **direct
socket** route, not a dead daemon. Codex documents this as an expected
limitation for Go `net/http` clients that bypass loopback proxies:
https://github.com/openai/codex/blob/main/codex-rs/network-proxy/README.md

Go's standard environment proxy selection always skips loopback. This PR
therefore adds a **separate opt-in** `AO_CLI_PROXY_LOOPBACK=1` to the AO CLI
when launched inside a Codex reviewer. Under that opt-in, the CLI:

- reads the sandbox-injected `http_proxy` / `HTTP_PROXY`;
- rejects any proxy endpoint outside `localhost` / loopback, or any
  non-HTTP or authenticated proxy URL;
- explicitly routes **only its fixed AO daemon loopback API** via the proxy
  for both `/healthz` and `/api/v1/...` calls;
- performs the same service-and-PID validation on the health response;
- leaves all ordinary host CLI behavior unchanged when the switch is absent.

AO's `AO_CODEX_REVIEW_LOOPBACK_PROFILE=1` launch setting injects both the
named Codex profile and `AO_CLI_PROXY_LOOPBACK=1` for reviewer-local shell
commands. This is a workaround only for the Codex TUI reviewer path. It
does not change the daemon's listener or permit automatic merge.

**Validation using the already-created PR #4 worktree, never replacing the
running daemon or exiting the SSH shell:**

```bash
set +e
(
  cd "$HOME/ao-eval-lab/ao-pr4-check" &&
  git fetch origin pull/4/head &&
  git switch --detach FETCH_HEAD &&
  cd backend &&
  go test ./internal/cli ./internal/adapters/reviewer/codex &&
  go build -o "$HOME/ao-eval-lab/ao-pr4-cli" ./cmd/ao &&
  codex sandbox -P ao-review-loopback -- \
    env AO_CLI_PROXY_LOOPBACK=1 \
        AO_RUN_FILE="$HOME/.ao-orchestrator-test/running.json" \
        AO_DATA_DIR="$HOME/.ao-orchestrator-test/data" \
        "$HOME/ao-eval-lab/ao-pr4-cli" review ls path-of-reasoning-4 --json
)
echo "Verification exit code: $?"
```

If `AO_CLI_PROXY_LOOPBACK needs the sandbox's http_proxy or HTTP_PROXY`
appears, inspect whether Codex actually injects a proxy into this permission
profile. Do not hard-code an external proxy endpoint or disable isolation.

A successful read-only `review ls` is **not** sufficient to claim the
review feedback loop is working. A scratch AO instance must also validate
reviewer `POST` submission, GitHub review ID and verdict persistence,
duplicate handling, and restart recovery before enabling automatic review.

## Operational decisions

- Keep the daemon bound to `127.0.0.1`; never bind `0.0.0.0` just for this.
- Accept the single-user host loopback API risk explicitly; no new auth system.
- Keep PR #2 frozen and leave ordinary Worker permissions unchanged.
- Never automatically merge a PR; CI/reviewer results are evidence for the
  owner, not authority to merge.
- This only covers Codex **TUI** review launches/restores. Codex app-server
  Chat reviews have a separate runtime surface and must be tested separately.

References: [Codex permission profiles](https://learn.chatgpt.com/docs/permissions)
and [CLI reference](https://learn.chatgpt.com/docs/cli/reference).
