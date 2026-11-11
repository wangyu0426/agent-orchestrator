#!/usr/bin/env bash
# Real AO-launched Codex TUI reviewer canary. Only operates on a disposable
# Draft PR and a newly allocated scratch AO daemon (never the live :3301).
# Usage: bash scripts/ao-codex-review-tui-e2e.sh start|check|restore|stop
set -euo pipefail
umask 077

BASE="$HOME/ao-eval-lab"
CHECKOUT="$BASE/ao-pr4-check"
LAST="$BASE/.native-review-pr4-current"
PR_URL="https://github.com/wangyu0426/agent-orchestrator/pull/3"
PR_BRANCH="codex/ao-native-review-gateway-acceptance-20261110"
PR_HEAD="9a2f8e41eb8caf6116dc0b87eadf38f37adfcda3"
PROJECT_ID="native-review-pr4-e2e"

die() { printf 'BLOCKED: %s\n' "$*" >&2; exit 1; }
require() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

load_state() {
  [[ -f "$LAST" ]] || die "no scratch instance found; run start first"
  ROOT="$(cat "$LAST")"
  case "$ROOT" in "$BASE"/native-review-pr4.*) ;; *) die "refusing unexpected scratch path: $ROOT";; esac
  [[ -d "$ROOT" && -x "$ROOT/ao" && -f "$ROOT/port" ]] || die "incomplete scratch state"
  PORT="$(cat "$ROOT/port")"
  [[ "$PORT" =~ ^[0-9]+$ && "$PORT" -ge 1024 && "$PORT" -ne 3301 ]] || die "unsafe scratch port"
}

ao_scratch() {
  AO_PORT="$PORT" AO_DATA_DIR="$ROOT/data" AO_RUN_FILE="$ROOT/running.json" \
    "$ROOT/ao" "$@"
}

start() {
  require git
  require go
  require codex
  require gh
  require tmux
  require curl
  require python3
  [[ -d "$CHECKOUT/.git" || -f "$CHECKOUT/.git" ]] || die "missing PR #4 test worktree $CHECKOUT"
  gh auth status >/dev/null 2>&1 || die "gh is not authenticated"
  # This is a safe GET; a failed check means the Reviewer cannot post reviews.
  codex sandbox -P ao-review-loopback -- gh api user --jq '.login' >/dev/null ||
    die "Codex Reviewer profile cannot access authenticated api.github.com"

  mkdir -p "$BASE"
  [[ ! -f "$LAST" ]] || die "scratch instance already tracked at $LAST; check/stop it first"
  ROOT="$(mktemp -d "$BASE/native-review-pr4.XXXXXXXX")"
  printf 'Scratch root: %s\n' "$ROOT"

  (
    cd "$CHECKOUT"
    git fetch origin pull/4/head
    git switch --detach FETCH_HEAD
    cd backend
    go test -count=1 ./internal/cli ./internal/adapters/reviewer/codex
    go build -o "$ROOT/ao" ./cmd/ao
  )

  git clone --quiet --branch "$PR_BRANCH" \
    https://github.com/wangyu0426/agent-orchestrator.git "$ROOT/project"
  git -C "$ROOT/project" fetch --quiet origin main:refs/remotes/origin/main
  actual_head="$(git -C "$ROOT/project" rev-parse HEAD)"
  [[ "$actual_head" == "$PR_HEAD" ]] ||
    die "fixture PR head changed: $actual_head (expected $PR_HEAD)"

  PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
  [[ "$PORT" != 3301 ]] || die "unexpected live AO port"
  printf '%s\n' "$PORT" > "$ROOT/port"
  # Only publish a resumable scratch instance after its binary and port exist.
  printf '%s\n' "$ROOT" > "$LAST"

  # Do not inherit an ambient reviewer-specific client proxy into the daemon.
  nohup env -u AO_CLI_PROXY_LOOPBACK \
    AO_PORT="$PORT" AO_DATA_DIR="$ROOT/data" AO_RUN_FILE="$ROOT/running.json" \
    AO_CODEX_REVIEW_LOOPBACK_PROFILE=1 AO_TELEMETRY_REMOTE=off \
    "$ROOT/ao" daemon >"$ROOT/daemon.log" 2>&1 </dev/null &
  scratch_pid="$!"
  printf '%s\n' "$scratch_pid" >"$ROOT/daemon-pid"
  completed=0
  cleanup_if_failed() {
    if [[ "$completed" != 1 ]]; then
      printf 'Scratch setup stopped before completion; attempting scoped daemon stop.\n' >&2
      ao_scratch stop >/dev/null 2>&1 || true
    fi
  }
  trap cleanup_if_failed EXIT

  ready=0
  for _ in $(seq 1 60); do
    if curl --noproxy '*' -fsS --max-time 2 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 0.25
  done
  if [[ "$ready" != 1 ]]; then
    tail -n 50 "$ROOT/daemon.log" >&2 || true
    die "scratch daemon failed to become ready"
  fi
  ao_scratch project add --path "$ROOT/project" --id "$PROJECT_ID" --worker-agent codex
  # The spawned worker worktree must start at the disposable PR head, not main.
  ao_scratch project set-config "$PROJECT_ID" --default-branch "$PR_BRANCH"

  spawn_output="$(ao_scratch spawn --project "$PROJECT_ID" --agent codex \
    --mode tui --name "Native Review #4 canary" --no-takeover \
    --claim-pr "$PR_URL" \
    --prompt "Acceptance test worker: do not modify the repository, do not run code, and remain idle. Reviewer is triggered separately.")"
  printf '%s\n' "$spawn_output"
  WORKER="$(printf '%s\n' "$spawn_output" |
    sed -n 's/^spawned session \([^ ]*\) .*/\1/p' | head -n 1)"
  [[ -n "$WORKER" ]] || die "failed to parse scratch worker session"
  printf '%s\n' "$WORKER" > "$ROOT/worker"

  # Exactly one additional worktree should have been created by AO.
  git -C "$ROOT/project" worktree list --porcelain |
    sed -n 's/^worktree //p' | grep -Fxv "$ROOT/project" > "$ROOT/worktree-list" || true
  [[ "$(wc -l < "$ROOT/worktree-list")" -eq 1 ]] ||
    die "expected exactly one worker worktree; inspect $ROOT/worktree-list"
  WORKTREE="$(cat "$ROOT/worktree-list")"
  printf '%s\n' "$WORKTREE" >"$ROOT/worktree"
  wt_head="$(git -C "$WORKTREE" rev-parse HEAD)"
  [[ "$wt_head" == "$PR_HEAD" ]] ||
    die "worker checkout is not the disposable PR HEAD ($wt_head)"

  # Before triggering a real Reviewer, prove its named profile cannot write
  # even a harmless canary file into the worker checkout.
  probe="$WORKTREE/.ao-tui-readonly-probe-$$"
  [[ ! -e "$probe" ]] || die "probe file already exists"
  if codex sandbox -P ao-review-loopback -- sh -c 'printf blocked > "$1"' _ "$probe"; then
    rm -f -- "$probe"
    die "reviewer permission profile allowed a repository write"
  fi
  [[ ! -e "$probe" ]] || die "readonly probe unexpectedly exists"
  [[ -z "$(git -C "$WORKTREE" status --porcelain --untracked-files=all)" ]] ||
    die "worker checkout is already dirty before Review; aborting"

  printf '\nTriggering REAL Codex TUI Reviewer on disposable Draft PR #3...\n'
  ao_scratch review trigger "$WORKER" --agent codex --interface tui \
    --pr "$PR_URL" --no-inject
  completed=1
  printf '\nStarted. Inspect with: bash scripts/ao-codex-review-tui-e2e.sh check\n'
  printf 'Stop ONLY scratch instance: bash scripts/ao-codex-review-tui-e2e.sh stop\n'
  printf 'Scratch root: %s\n' "$ROOT"
}

check() {
  require python3
  require gh
  load_state
  [[ -f "$ROOT/worker" && -f "$ROOT/worktree" ]] || die "scratch worker has not been provisioned"
  WORKER="$(cat "$ROOT/worker")"
  WORKTREE="$(cat "$ROOT/worktree")"
  ao_scratch status --json
  review_json="$(ao_scratch review ls "$WORKER" --json)"
  printf '%s\n' "$review_json"
  review_id="$(printf '%s\n' "$review_json" | python3 -c '
import json,sys
d=json.load(sys.stdin)
v=next((r for r in d.get("reviews",[]) if r.get("prNumber")==3),None)
if not v: sys.exit("BLOCKED: fixture PR #3 missing from AO review list")
r=v.get("latestRun") or {}
if r.get("targetSha") != "9a2f8e41eb8caf6116dc0b87eadf38f37adfcda3":
    sys.exit("BLOCKED: review run is not pinned to fixture HEAD")
if r.get("status") not in ("complete","delivered"):
    sys.exit("PENDING: AO review run has not completed (status=%s)"%r.get("status"))
if r.get("verdict") not in ("approved","changes_requested"):
    sys.exit("BLOCKED: AO completed run lacks a valid internal verdict")
print(r.get("githubReviewId",""))
')"
  [[ -n "$review_id" ]] || die "AO has not recorded a real GitHub Review ID yet (may still be running)"
  gh api "repos/wangyu0426/agent-orchestrator/pulls/3/reviews/$review_id" |
    python3 -c '
import json,sys
r=json.load(sys.stdin)
assert r.get("commit_id")=="9a2f8e41eb8caf6116dc0b87eadf38f37adfcda3",r
assert r.get("state")=="COMMENTED",r
print("Provider review verified: id=%s commit=%s state=%s"%(r["id"],r["commit_id"],r["state"]))
'
  [[ "$(git -C "$WORKTREE" rev-parse HEAD)" == "$PR_HEAD" ]] ||
    die "reviewer checkout HEAD changed"
  [[ -z "$(git -C "$WORKTREE" status --porcelain --untracked-files=all)" ]] ||
    die "reviewer checkout was modified; inspect before cleanup"
  printf 'Verified provider review ID, exact PR head, and clean read-only checkout.\n'
  printf 'Scratch run status must show completed verdict above; do not confuse provider COMMENT with AO internal verdict.\n'
}


# Restore acceptance after the completed review. This is intentionally scoped
# to one pinned scratch test and refuses to signal any process except the
# reviewer PTY host recorded in this test instance's own registry.
restore() {
  require python3
  require curl
  require gh
  require git
  load_state
  [[ -f "$ROOT/worker" && -f "$ROOT/worktree" ]] || die "scratch worker metadata missing"
  WORKER="$(cat "$ROOT/worker")"
  WORKTREE="$(cat "$ROOT/worktree")"
  [[ "$WORKER" == "native-review-pr4-e2e-1" ]] ||
    die "refusing to restore an unexpected worker $WORKER"
  [[ ! -e "$ROOT/restore-attempted" ]] ||
    die "restore already attempted; inspect $ROOT/restore-* before retrying"
  [[ -f "$ROOT/windows-pty-hosts.json" ]] ||
    die "scratch PTY registry missing; refusing to signal any process"
  ao_scratch status --json >"$ROOT/restore-daemon-before.json"
  check
  gh api repos/wangyu0426/agent-orchestrator/pulls/3/reviews >"$ROOT/restore-github-before.json"

  # Preflight uses SQLite read-only mode. The exact completed run, real
  # provider review ID and hook-captured native Codex session are mandatory.
  # It also checks /proc/exe AND /proc/cmdline match this scratch binary.
  python3 - "$ROOT" "$WORKER" "$PR_HEAD" <<'PY'
import json, pathlib, sqlite3, sys
root, worker, head = sys.argv[1:]
root = pathlib.Path(root)
expected_handle = "review-" + worker
db = sqlite3.connect(f"file:{root / 'data' / 'ao.db'}?mode=ro", uri=True)
rv = db.execute(
    "SELECT id, agent_session_id, reviewer_handle_id, reviewer_launch_id, interface_mode "
    "FROM review WHERE session_id=? AND harness='codex' AND is_archived=0", (worker,)
).fetchall()
if len(rv)!=1:
    sys.exit("BLOCKED: expected exactly one non-archived Codex reviewer row")
review_id, native_id, handle, generation, mode = rv[0]
if not native_id or not generation or mode != 'tui' or expected_handle not in handle:
    sys.exit("BLOCKED: TUI reviewer has no saved native Codex session/launch ID or handle; will not kill PTY")
runs = db.execute(
    "SELECT id, status, verdict, github_review_id, target_sha "
    "FROM review_run WHERE session_id=?", (worker,)
).fetchall()
run_id = "558e2883-11ad-4ecc-a9ea-90ef534e3daa"
if len(runs)!=1 or runs[0] != (run_id,'complete','approved','5860205489',head):
    sys.exit(f"BLOCKED: unexpected review runs before restoration: {runs!r}")
registry = json.loads((root / "windows-pty-hosts.json").read_text())
matches = [x for x in registry if x.get("sessionId") == expected_handle]
if len(matches)!=1:
    sys.exit("BLOCKED: exactly one scratch reviewer PTY host entry required")
pid = matches[0]["ptyHostPid"]
daemon_pid = json.loads((root / "running.json").read_text())["pid"]
status = json.loads((root / "restore-daemon-before.json").read_text())
if (status.get("state") != "ready" or status.get("pid") != daemon_pid or
    status.get("port") != int((root / "port").read_text()) or
    pathlib.Path(status.get("executablePath","")).resolve() != (root/"ao").resolve()):
    sys.exit("BLOCKED: scratch daemon identity/port/binary mismatch")
if not isinstance(pid,int) or pid <= 1 or pid == daemon_pid:
    sys.exit("BLOCKED: invalid or daemon PTY PID")
proc = pathlib.Path(f"/proc/{pid}")
try:
    exe = (proc/"exe").resolve(strict=True)
    argv = (proc/"cmdline").read_bytes().rstrip(b"\0").split(b"\0")
except (OSError, ValueError) as e:
    sys.exit(f"BLOCKED: PTY host process unavailable: {e}")
expected_bin = (root/"ao").resolve(strict=True)
args = [a.decode("utf-8", "replace") for a in argv]
if exe != expected_bin or len(args)<3 or args[1] != "pty-host" or expected_handle not in args:
    sys.exit(f"BLOCKED: PTY process identity mismatch; executable={exe}, args={args[:5]}")
info = dict(
    worker=worker, reviewId=review_id, nativeSessionId=native_id,
    reviewerHandle=handle, generation=generation, pid=pid,
    githubReviewId="5860205489", reviewRunId=run_id
)
(root/"restore-before.json").write_text(json.dumps(info, indent=2)+"\n")
print("PRECHECK PASS: isolated reviewer PTY host=%s, saved native Codex session present, completed review pinned" % pid)
PY

  # The once-only marker prevents blind repeated SIGTERM after a partial
  # restore. No arbitrary pgrep, pkill, process-group kill or daemon restart.
  touch "$ROOT/restore-attempted"
  old_pid="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["pid"])' "$ROOT/restore-before.json")"
  printf 'SIGTERM only to verified scratch Reviewer PTY host PID %s\n' "$old_pid"
  # pidfd binds the signal to the exact process identity, even if a PID gets
  # recycled between the preflight snapshot and the signal attempt. Linux only.
  python3 - "$ROOT" "$old_pid" "$WORKER" <<'PY'
import json, os, pathlib, signal, sys
root, pid, worker = pathlib.Path(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
before = json.loads((root/"restore-before.json").read_text())
if before["pid"] != pid or before["worker"] != worker:
    sys.exit("BLOCKED: restore PID/worker preflight mismatch")
try:
    fd = os.pidfd_open(pid)
    exe = pathlib.Path(f"/proc/{pid}/exe").resolve(strict=True)
    args = pathlib.Path(f"/proc/{pid}/cmdline").read_bytes().rstrip(b"\0").split(b"\0")
    expected = (root/"ao").resolve(strict=True)
    if exe != expected or len(args)<3 or args[1]!=b"pty-host" or ("review-"+worker).encode() not in args:
        sys.exit("BLOCKED: reviewer host identity changed; refusing SIGTERM")
    signal.pidfd_send_signal(fd, signal.SIGTERM)
except (OSError, ValueError) as e:
    sys.exit(f"BLOCKED: cannot safely signal reviewer PID: {e}")
finally:
    if "fd" in locals():
        os.close(fd)
PY
  gone=0
  for _ in $(seq 1 30); do
    if [[ ! -e "/proc/$old_pid" ]] ||
      [[ "$(awk '{print $3}' "/proc/$old_pid/stat" 2>/dev/null)" == "Z" ]]; then
      gone=1
      break
    fi
    sleep 0.25
  done
  [[ "$gone" == 1 ]] || die "reviewer PTY did not exit; inspect scratch instance (no force-kill)"

  printf 'Restoring Reviewer through scratch AO /reviews/restore API...\n'
  curl --noproxy '*' -fsS --max-time 45 \
    -H "Content-Type: application/json" -d '{}' \
    "http://127.0.0.1:$PORT/api/v1/sessions/$WORKER/reviews/restore" \
    > "$ROOT/restore-response.json" ||
      die "restore API failed; inspect $ROOT/daemon.log and restore-response.json"
  cat "$ROOT/restore-response.json"
  printf '\n'

  # A new PTY host must own the same stable reviewer handle, with a new
  # generation. AO must preserve the native session and completed review run.
  python3 - "$ROOT" "$WORKER" <<'PY'
import json, pathlib, sqlite3, sys
root, worker = pathlib.Path(sys.argv[1]), sys.argv[2]
before = json.loads((root/"restore-before.json").read_text())
db = sqlite3.connect(f"file:{root / 'data' / 'ao.db'}?mode=ro", uri=True)
rv = db.execute(
    "SELECT agent_session_id, reviewer_handle_id, reviewer_launch_id, interface_mode "
    "FROM review WHERE session_id=? AND harness='codex' AND is_archived=0", (worker,)
).fetchone()
if rv is None:
    sys.exit("BLOCKED: reviewer row missing after restore")
if before['reviewerHandle'].startswith('ptyhost-v1:') and not rv[1].startswith('ptyhost-v1:'):
    sys.exit("BLOCKED: native PTY restore fell back to legacy tmux: %r"% (rv,))
if rv[0] != before['nativeSessionId']:
    sys.exit("BLOCKED: Codex native session ID changed during restore")
if rv[1] != before['reviewerHandle']:
    sys.exit("BLOCKED: reviewer runtime handle changed: %r"% (rv,))
if rv[2] == before['generation'] or rv[3] != 'tui':
    sys.exit("BLOCKED: reviewer launch generation/mode did not change as required: %r"% (rv,))
runs = db.execute(
    "SELECT id, status, verdict, github_review_id FROM review_run WHERE session_id=?", (worker,)
).fetchall()
if runs != [(before['reviewRunId'],'complete','approved',before['githubReviewId'])]:
    sys.exit("BLOCKED: reviewer restore created or changed Review Run: %r"% (runs,))
entries = json.loads((root/"windows-pty-hosts.json").read_text())
matches = [e for e in entries if e.get("sessionId")=="review-"+worker]
if len(matches)!=1 or matches[0]["ptyHostPid"]==before["pid"]:
    sys.exit("BLOCKED: new native Reviewer PTY not created")
print("RESTORE PASS: nativeSessionId preserved; Reviewer generation and PTY replaced; completed Review Run unchanged")
PY
  gh api repos/wangyu0426/agent-orchestrator/pulls/3/reviews >"$ROOT/restore-github-after.json"
  python3 - "$ROOT" <<'PY'
import json,sys,pathlib
r=pathlib.Path(sys.argv[1])
before=json.loads((r/"restore-github-before.json").read_text())
after=json.loads((r/"restore-github-after.json").read_text())
if sorted(x["id"] for x in before)!=sorted(x["id"] for x in after):
    sys.exit("BLOCKED: Reviewer restore caused a GitHub Review count/ID change")
print("GITHUB PASS: no duplicate provider Review created during native resume")
PY
  check
  printf '\nNative TUI Reviewer restore acceptance PASS (scratch only).\n'
}

stop() {
  load_state
  printf 'Stopping scratch AO daemon on port %s (never 3301)\n' "$PORT"
  ao_scratch stop
  rm -f -- "$LAST"
  printf 'Test files retained at %s for evidence. Disposable PR #3 remains Draft/Open; close it after review.\n' "$ROOT"
}

[[ "$#" -eq 1 ]] || die "usage: bash scripts/ao-codex-review-tui-e2e.sh start|check|restore|stop"
case "$1" in
  start) start ;;
  check) check ;;
  restore) restore ;;
  stop) stop ;;
  *) die "usage: start|check|restore|stop" ;;
esac
