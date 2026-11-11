#!/usr/bin/env bash
# Real AO-launched Codex TUI reviewer canary. Only operates on a disposable
# Draft PR and a newly allocated scratch AO daemon (never the live :3301).
# Usage: bash scripts/ao-codex-review-tui-e2e.sh start|check|stop
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
  printf '%s\n' "$ROOT" > "$LAST"
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

stop() {
  load_state
  printf 'Stopping scratch AO daemon on port %s (never 3301)\n' "$PORT"
  ao_scratch stop
  rm -f -- "$LAST"
  printf 'Test files retained at %s for evidence. Disposable PR #3 remains Draft/Open; close it after review.\n' "$ROOT"
}

[[ "$#" -eq 1 ]] || die "usage: bash scripts/ao-codex-review-tui-e2e.sh start|check|stop"
case "$1" in
  start) start ;;
  check) check ;;
  stop) stop ;;
  *) die "usage: start|check|stop" ;;
esac
