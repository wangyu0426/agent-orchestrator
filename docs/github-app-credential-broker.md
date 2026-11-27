# Repo-Scoped GitHub App Credential Broker for Workers

The worker credential broker extends [GitHub App Authentication for AO Daemon](github-app-auth.md) so worker `git push` and `gh` API/PR operations authenticate as the GitHub App installation instead of the operator's personal credentials.

## Architecture & Trust Boundary

- **Daemon-side only**: The GitHub App RSA private key and `githubapp.Provider` remain strictly inside the AO daemon process.
- **Per-session capability**: Each worker session receives an unguessable per-session bearer token (`AO_GITHUB_BROKER_TOKEN`) bound to its `AO_SESSION_ID` and registered project repository identities (`owner/repo`). The daemon stores only a constant-time-verified SHA-256 digest bound to the session ID.
- **Loopback-only HTTP API**: Workers request short-lived tokens on demand via `POST /internal/github/broker/token` (and evict rejected tokens via `POST /internal/github/broker/invalidate`). Both routes are blocked on the LAN/mobile listener (`lanControlBlock` on `/internal/`) and require loopback host validation plus `Authorization: Bearer <AO_GITHUB_BROKER_TOKEN>`.
- **Least-privilege, single-repository tokens**: The broker mints installation access tokens scoped to a single registered repository (verified against both the session's registered repo(s) and `AO_GITHUB_APP_REPOSITORIES`) with least-privilege permissions:
  - `contents: write`
  - `pull_requests: write`
  - `issues: write`
  - `metadata: read`
  - `checks: read`
  - `statuses: read`
  - Administrative, workflow, and organization permissions are excluded.
- **No static `GH_TOKEN` & no global `gh` mutation**:
  - Git operations use `ao github-broker git-credential` (`get` fetches/refreshes on demand, `store` is a no-op, `erase` invalidates the cached token).
  - `gh` operations run through a session-scoped `gh` shim (`ao github-broker gh`) that fetches a fresh token on demand, injects `GH_TOKEN` into only that single child `gh` process, and sets `GH_CONFIG_DIR` to an isolated temporary directory so `~/.config/gh` is never read or mutated.
  - Sessions exceeding the 1-hour GitHub App token lifetime refresh transparently.

## Configuration

The worker broker is **opt-in** and requires `AO_GITHUB_APP_ENABLED=on`.

| Variable | Required | Description |
| --- | --- | --- |
| `AO_GITHUB_APP_WORKER_BROKER_ENABLED` | Yes (to enable broker) | Set to `on` / `true` / `1`. Fails closed if `AO_GITHUB_APP_ENABLED` is not `on`. |
| `AO_GITHUB_APP_INSTALLATION_OWNER` | Yes (when broker enabled) | GitHub org/account owner for the installation, used to bind `AO_GITHUB_APP_REPOSITORIES` to canonical `owner/repo` identities. |
| `AO_GITHUB_APP_WORKER_ISOLATION` | No (default `strict`) | Isolation verification mode: `strict`, `external-sandbox`, or `unsafe-same-uid`. |
| `AO_GITHUB_APP_HOST` | No (default `github.com`) | GitHub host served by the broker. |

All `AO_GITHUB_APP_*` variables are scrubbed from the daemon environment at startup via `config.ScrubGitHubAppEnv()`.

## OS & Sandbox Isolation (Fail-Closed)

Same-UID `chmod 600` alone is **insufficient**: if the AO daemon and worker processes run under the same OS UID on the same host without an OS/container sandbox, any worker can read `AO_GITHUB_APP_PRIVATE_KEY_PATH` directly.

- **`strict` (default)**: Fails closed at daemon startup (`ErrUnsupportedIsolation`) if `AO_GITHUB_APP_PRIVATE_KEY_PATH` is readable by the daemon/worker UID.
- **`external-sandbox`**: Use when workers run inside a verified OS, container, or VM sandbox (e.g., separate UID/service user, Docker/Podman container without the key bind-mounted, macOS Seatbelt/Linux AppArmor/nsjail) that prevents workers from accessing the daemon's private key path.
- **`unsafe-same-uid`**: Explicit local development and operator-approved smoke-test override only. Logs a warning at startup. Do not use in untrusted multi-tenant or production environments.

## Inventory: Remotes, Inherited Credentials, Helper Bypasses & Launch Paths

When broker mode is active, `githubbroker.WorkerEnv` enforces the following across all worker launch paths (tmux/ConPTY terminal sessions, Codex, AGY, Claude Code, and scoped `shellterm` shells):

| Vector | Mitigation |
| --- | --- |
| **SSH remotes & agent** (`git@github.com:...`, `ssh://git@github.com/...`, `SSH_AUTH_SOCK`, `GIT_SSH`, `GIT_SSH_COMMAND`) | Environment-only Git config `url.https://<host>/.insteadOf` rewrites SSH remotes to HTTPS without mutating `.git/config` or `~/.gitconfig`; `SSH_AUTH_SOCK`, `GIT_SSH`, and `GIT_SSH_COMMAND` are scrubbed from worker environments. |
| **Inherited personal tokens** (`GH_TOKEN`, `GITHUB_TOKEN`, `AO_GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`, `GITHUB_ENTERPRISE_TOKEN`) | Explicitly scrubbed to empty in worker env and in every `ao github-broker gh` child process. |
| **Credential helper bypasses** (system/global `credential.helper`, `GIT_ASKPASS`, `SSH_ASKPASS`) | `GIT_CONFIG_KEY_0=credential.helper` with empty value clears inherited helpers; `GIT_CONFIG_KEY_1` installs `!ao github-broker git-credential`; `credential.useHttpPath=true` ensures repo path validation; `GIT_ASKPASS`/`SSH_ASKPASS` are scrubbed and `GIT_TERMINAL_PROMPT=0`. |
| **Global `gh` auth state** (`~/.config/gh/hosts.yml`) | `GH_CONFIG_DIR` is pointed to an isolated empty directory; global `gh` auth is never read or mutated. |
| **Commit author metadata vs push actor** | `GIT_AUTHOR_NAME`/`GIT_AUTHOR_EMAIL` and `GIT_COMMITTER_*` are untouched. Commits retain their configured author metadata while the authenticated GitHub push/PR actor is the GitHub App installation (`<app-slug>[bot]`). |

## Preserved Invariants

- **Legacy auth mode**: When `AO_GITHUB_APP_WORKER_BROKER_ENABLED` is unset/off, existing `AO_GITHUB_TOKEN` / `GITHUB_TOKEN` / `gh auth` behavior is unchanged.
- **Human final merge gate & branch protection**: The broker does not add any automatic merge path and respects repository branch protection rules.
- **Feedback routing**: Human review feedback continues to route to workers; App-authored status comments do not self-loop.

## Migration & Rollback

Production deployment must be operator-approved (never performed automatically by a worker).

### Rollout / Live Smoke Verification Checklist

1. Configure `AO_GITHUB_APP_ENABLED=on` and `AO_GITHUB_APP_WORKER_BROKER_ENABLED=on` with an appropriate `AO_GITHUB_APP_WORKER_ISOLATION` setting.
2. Verify **Codex** and **AGY** workers separately:
   - Push a branch and create a PR as the GitHub App installation (including after a token refresh/expiry).
   - Confirm attempts to access another repository or read the private key fail closed.
   - Confirm human review feedback reaches the worker and App-generated status updates do not loop.
   - Confirm branch protection is enforced and no automatic merge path exists.

### Rollback

1. Unset `AO_GITHUB_APP_WORKER_BROKER_ENABLED` (or set it to `off`) to revert worker credentials while keeping daemon App auth, or also unset `AO_GITHUB_APP_ENABLED` to return fully to legacy PAT/`gh` mode.
2. Restart the AO daemon.
3. Restart/relaunch any live worker sessions that were started while broker mode was enabled. Because daemon startup adopts surviving worker runtimes, those existing processes still carry the broker `PATH` shim and scrubbed personal credentials in their environment and will receive `503` from the disabled broker until relaunched.
4. Verify newly launched workers resume using the configured legacy credential chain.
