# Upstream stable v0.15.5 integration

This is the integration record for [fork issue #17](https://github.com/wangyu0426/agent-orchestrator/issues/17).
It describes the reviewed upstream target, the fork-only behavior carried through
the merge, the related independent issues, and the safe upgrade path for the
self-hosted `ao-self-hosted.service` install. No live host, session, database, or
installed binary was used during this integration.

## Pinned source and merge

- Upstream: `OrchestratorInc/agent-orchestrator`, stable tag `v0.15.5`, commit
  [`523efd8ac08aea75e62f3d31e6e669fde8743123`](https://github.com/OrchestratorInc/agent-orchestrator/commit/523efd8ac08aea75e62f3d31e6e669fde8743123).
  The official tag ref points directly to this commit. The release was published
  on 2026-11-23.
- Fork main before integration: `763e94c9beb6fddd6eae778e88817709a79e0017`.
- Merge base: `f54d5cc3cca456bb880214d6c9736f4f2420d22e`.
- At the start of this integration, the range from the merge base contained two
  fork-only commits and 297 upstream-only commits. The initial fork-only commits
  are `f0deef47` (Codex Reviewer loopback profile and native PTY restore) and
  `763e94c9` (scoped engineering memory and review guidance).
- During validation, `origin/main` advanced from `763e94c9` to
  `1326add3544caa140729c20d837d624b37dadf20`, which only adds the independent
  issue delivery policy to `AGENTS.md`. That main update is merged separately
  after the upstream tag merge; the final fork-only inventory from the common
  merge base also includes `1326add35`. No open draft PR #1, #2, or #9 is an
  input.
- Integration uses normal merges from `v0.15.5` and current `main`; no force
  push, rebase, or history rewrite. The upstream tag merge had no textual
  conflicts.
  Both sides changed `AGENTS.md`, `backend/internal/cli/client.go`,
  `backend/internal/cli/e2e_test.go`, `backend/internal/review/launcher.go`,
  `backend/internal/review/launcher_test.go`, and
  `backend/internal/review/review.go`. The Codex loopback and exact persisted
  PTY-handle restore behavior was retained and regression-tested; the engineering
  memory section remains in `AGENTS.md` alongside upstream guidance.
- Open draft PRs #1, #2, and #9 are not merge inputs and remain independent.
- As of 2026-11-24, `v0.15.5` is the latest stable, non-prerelease release. The
  later `v0.15.6-nightly.202611240103` tag is a nightly prerelease and is not the
  target of this integration.

## Fork behavior carried forward

The following fork-specific behavior remains in the merge result:

- Tracker intake remains gated by `AO_TRACKER_INTAKE` (off by default) and each
  project's `trackerIntake.enabled` setting. Enabled intake requires an assignee
  rule, starts workers through the existing spawn service, canonicalizes tracker
  issue identity, and skips issues already represented by non-terminated
  sessions. The current-main config, observer, daemon wiring, and tests are
  unchanged from the fork side of the merge.
- The Codex Reviewer loopback profile stays opt-in through
  `AO_CODEX_REVIEW_LOOPBACK_PROFILE=1`. Only the reviewer receives the named
  `ao-review-loopback` Codex permission profile. Its CLI proxy opt-in still
  requires an unauthenticated HTTP proxy bound to loopback and pins requests to
  the daemon's loopback URL; non-loopback endpoints are rejected. Worker
  launches are unchanged, and the reviewer's read-only filesystem sandbox is
  retained.
- Native PTY reviewer recovery destroys the exact persisted versioned handle
  (for example, `ptyhost-v1:review-…`) before creating a replacement. It does not
  route cleanup through the bare legacy tmux name.
- Project/session configuration and the existing CLI, daemon, and systemd
  service boundary were merged without changing user project settings. Review
  permissions and lifecycle safety remain governed by the merged Codex policy
  and existing tests.

## Verified upstream behavior in the target tag

These changes are ancestors of the pinned tag and are present in its source.

| Upstream change | Source evidence and behavior in `v0.15.5` |
| --- | --- |
| [#8407 browser cleanup](https://github.com/OrchestratorInc/agent-orchestrator/pull/8407), `736a3796` | `frontend/src/main/browser-storage-cleanup.ts` and tests remove only UUID-named temporary browser partitions under Electron `userData`, clear the temporary profile V8 cache, and run a one-time root-cache purge. Named browser profiles are preserved. |
| [#8400 idle Chromium resources](https://github.com/OrchestratorInc/agent-orchestrator/pull/8400), `adf9f6a3` | `frontend/src/main/browser-view-host.ts` throttles hidden/idle pages. It restores foreground activity while visible or while agent commands, streams, DevTools/network capture, or audio need the page. |
| [#8016 AGY ACP](https://github.com/OrchestratorInc/agent-orchestrator/pull/8016), `ea017f2e` | `backend/internal/adapters/chatdriver/agyacp/` selects built-in `agy acp` with steering for CLI 1.5.4 and later. Older CLIs and explicit `AO_AGY_ACP_SERVER_PATH` use the separate-server path; approval mapping remains enforced. |
| [#8355 account selection](https://github.com/OrchestratorInc/agent-orchestrator/pull/8355), `db74eaec` | `ao account ls/default/show/switch` and `ao spawn --account` expose explicit account selection. New-session defaults and session account handoff are explicit user choices; quota detection does not silently reroute a worker. |
| [#8386 reviewer effort](https://github.com/OrchestratorInc/agent-orchestrator/pull/8386), `9f60abf0` | `ReviewerPicker` and `SessionInspector` let a reviewer effort be inherited, selected, persisted, cleared, and passed to the next review run, with supported values tied to the selected model/mode. |
| [#8195 ACP loss recovery](https://github.com/OrchestratorInc/agent-orchestrator/pull/8195), `fc423f98` | ACP treats the provider's JSON-RPC `-32603` “Session not found” response as a lost conversation: it stops the provider and closes the chat so it can be resumed. Generic internal errors and plain-text lookalikes are not classified as session loss, and no automatic retry/resume is started. |

## Relationship to fork issues #5, #14, #15, and #16

The sync does not implement or close these separate issues. These are partial
overlaps only; none blocks this upstream integration.

| Issue | Existing upstream overlap | Remaining issue scope after this sync |
| --- | --- | --- |
| [#5 Archive on Merge](https://github.com/wangyu0426/agent-orchestrator/issues/5) | Per-session `TerminateOnPRMerge` and the SCM observer can end a session after its PR merges. Existing cleanup is tied to session ownership/lifecycle. | Global/project defaults and precedence, safe worker process-tree cleanup policy, and preview/apply controls for batch archive remain separate work. |
| [#14 quota failover](https://github.com/wangyu0426/agent-orchestrator/issues/14) | Account listing/defaults/switching, manual `--account` selection, provider usage/limit readings, and account handoff primitives are present. | No authoritative quota-driven worker selection/failover policy, restart/defer behavior, or cross-provider automatic intake routing is added here. |
| [#15 session TMPDIR GC](https://github.com/wangyu0426/agent-orchestrator/issues/15) | #8407 reclaims temporary Chromium partitions and caches rooted under Electron `userData` (`~/.ao/electron`). | Session-scoped TMPDIR propagation and orphan collection are distinct. This sync does not sweep global `/tmp` or add session temporary-storage GC. |
| [#16 admission limits](https://github.com/wangyu0426/agent-orchestrator/issues/16) | Existing tracker deduplication and individual reviewer/start guards continue to operate. | No global/project resident worker/reviewer caps or deferred admission queue is added by this release sync. |

## SQLite upgrade and rollback

The fork-main fixture schema ends at migration
`20261107181854835_session_delete_child_indexes.sql`; the target reaches
`20261115072651599_allow_minimax_code_harness.sql`. The 24 migration files
introduced since the merge base are:

```text
20261028180000000_pr_details.sql
20261028224250364_usage_event_service_tier.sql
20261028233910979_agent_limit_readings.sql
20261030230920365_chat_account_handoff.sql
20261030234657125_app_settings_limit_history.sql
20261031173315209_usage_event_provider_message_id.sql
20261031173316629_usage_long_context_reprice.sql
20261101102149555_pr_details_conversation.sql
20261103000000000_session_import.sql
20261103030401452_open_usage_source_kinds.sql
20261105105237146_usage_repricing.sql
20261109072438921_terminal_titles.sql
20261109094334974_session_artifacts.sql
20261109120000000_conversation_turn_batch_delivery.sql
20261109153109361_session_forks.sql
20261110172911382_conversation_turn_artifacts.sql
20261111145612313_pr_details_provider_updated_at.sql
20261113111818788_clear_switched_model_reroutes.sql
20261113173435236_session_search.sql
20261114072016988_drop_session_account_pins.sql
20261114182101000_session_accepted_delivery.sql
20261115050423095_allow_stakpak_harness.sql
20261115071147951_allow_docker_agent_harness.sql
20261115072651599_allow_minimax_code_harness.sql
```

Goose runs missing out-of-order migrations as well as later versions in the
production `Open` path. The fixture starts from the fork-main schema with a
representative project/tracker intake config and session identity, reviewer
settings, runtime handle, and provider recovery identifiers. It verifies those
values after production migration and checks the migration receipts. A restore
test writes the pre-upgrade SQLite snapshot back and verifies its schema/data
version, modeling the database portion of rollback.

Some Down migrations cannot restore dropped data. In particular,
`20261114072016988_drop_session_account_pins.sql` recreates an empty pins table;
other migrations rebuild constrained usage tables, remove unsupported usage
rows on Down, or drop derived tables/columns. Therefore **do not roll back by
switching only the binary**. Restore a backup of the pre-upgrade database and
the previous release together. Any work recorded after that backup is lost on
restore. The tag has Goose Down sections, but that does not make this deployment
binary-only reversible.

## Build, deploy, verify, and roll back

These commands describe a future owner-approved deployment to the existing
Linux self-hosted user service. They were not run against `yuworker`.

Build the Linux host bundle from the reviewed commit on a Linux x64 build host:
use Go `1.27.4`, Node `22.23.5`, and install `build-essential`, `bison`,
`pkg-config`, and `xz-utils` first. `build:tmux` compiles pinned native sources
and requires `yacc`, which the `bison` package provides. The self-hosted source
bootstrap now installs that dependency as well.

```bash
sudo apt-get update
sudo apt-get install -y build-essential bison pkg-config xz-utils
cd frontend
npm ci
npm run build:daemon
npm run build:tmux
npm run build:acp-runtime
npm run build:host
# Bundle: frontend/dist-host/ao-host-linux-x64.tar.gz
```

Copy that bundle to the AO user's home directory on the host, and copy the
reviewed `scripts/setup-self-hosted.sh` there as `setup-self-hosted.sh`. In the
AO user's login shell, inspect
`~/.ao/host/run-daemon.sh` and preserve the existing `AO_HOST_INSTALL_DIR`,
`AO_DATA_DIR`, `AO_RUN_FILE`, and `AO_PORT` values (and the service PATH) when
invoking the installer. If a value was not explicitly present in the old runner,
leave it unset so its existing default remains in effect. The installer writes
a new runner from the invoking environment.

```bash
host_root="${AO_HOST_INSTALL_DIR:-$HOME/.ao/host}"
old_release="$(readlink -f "$host_root/current")"
printf 'Previous release: %s\n' "$old_release"
systemctl --user status ao-self-hosted.service --no-pager
sed -n '/^export AO_/p; /^export PATH=/p' "$host_root/run-daemon.sh"
```

After recording the existing runner settings, stop the service and create a
consistent SQLite backup. Use the real `AO_DATA_DIR` from the runner when it is
set; otherwise it defaults to `~/.ao/data`.

```bash
systemctl --user stop ao-self-hosted.service
data_dir="${AO_DATA_DIR:-$HOME/.ao/data}"
backup="$HOME/ao-pre-v0155-$(date +%Y%m%d%H%M%S).db"
python3 - "$data_dir/ao.db" "$backup" <<'PY'
import sqlite3, sys
source = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
target = sqlite3.connect(sys.argv[2])
source.backup(target)
target.close()
source.close()
PY
```

Run setup as the AO user, with the saved host-root and any previously exported
`AO_DATA_DIR`, `AO_RUN_FILE`, and `AO_PORT` values restored in the environment:
if the old install used a custom `AO_HOST_INSTALL_DIR`, export that exact value;
leave it unset for the default `~/.ao/host` root.

```bash
./setup-self-hosted.sh --bundle "$HOME/ao-host-linux-x64.tar.gz"
systemctl --user status ao-self-hosted.service --no-pager
journalctl --user -u ao-self-hosted.service -n 100 --no-pager
"$host_root/current/resources/daemon/ao" status
```

The installer stages a versioned release, switches `current`, regenerates the
service runner, and restarts the user unit. Its unit intentionally uses
`KillMode=process`, so stopping the daemon does not signal its worker processes.
This sync was not used to deploy or replace the installed `yuworker` binary.

If validation fails, restore the saved database and the recorded release while
the service is stopped. Use the actual data directory and host root captured
above:

```bash
systemctl --user stop ao-self-hosted.service
cp "$backup" "$data_dir/ao.db"
rm -f "$data_dir/ao.db-wal" "$data_dir/ao.db-shm"
ln -sfn "$old_release" "$host_root/current"
systemctl --user start ao-self-hosted.service
systemctl --user status ao-self-hosted.service --no-pager
journalctl --user -u ao-self-hosted.service -n 100 --no-pager
```

## Validation record

- Backend `go build ./...` and `go vet ./...` passed. The focused suites for
  CLI, reviewer/Codex permissions, tracker intake, ACP recovery, AGY ACP, and
  SQLite passed; the full SQLite package passed, including the disposable
  fork-main upgrade/restore fixture. The complete `go test ./...` run was
  attempted: the container's mapped filesystem ownership caused the Codex
  secure-path tests to reject an ancestor, its group-writable `~/.codex` was
  left untouched, and extracted fish/zsh packages lacked their normal system
  library/module paths. The fake lifecycle package passed on retry; the account
  storage and shell terminal packages need the required Linux CI environment.
- Frontend typecheck and E2E typecheck passed under Node 24. Both full Vitest
  shards ran under the available Node 22: 14,863 tests passed and 42 were
  skipped; Node 22 child-process TypeScript support and two parallel-load hook
  timeouts caused failures. Re-running the three affected files under Node
  24.22.3 passed all 195 tests. Product UI typecheck, 180 tests, and build
  passed. Mobile typecheck and all 4,254 tests passed under its CI Node 20
  runtime. Docs translation check, all 7 docs tests, and production build
  passed; translation checking reports the two existing stale quickstart
  translation-source warnings. Cloud-client generation was clean, typecheck,
  34 tests, and pack dry-run passed.
- The Linux self-hosted daemon, tmux, ACP runtime, and host bundle built
  successfully after extracting `bison` into `/tmp`; all 20 self-hosted setup
  fixture scenarios and the host-bundle fixture test passed. Config-lane Node
  tests passed (27/27). No live provider/quota calls were made. CLI E2E could
  not run because the Docker socket is unavailable; Electron boundary tests and
  native screenshots could not run because this host has no display/Xvfb and
  Chromium's sandbox helper is not configured for this container.
- No production `yuworker` commands or live data were used. Live-host systemd
  validation and rollback remain a separate owner-approved deployment
  follow-up.
