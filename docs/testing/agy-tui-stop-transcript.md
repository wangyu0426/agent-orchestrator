# AGY TUI Stop Transcript Ingestion Smoke Test (Opt-In)

This document describes an opt-in local smoke test for verifying AGY TUI completed-turn ingestion from `Stop.transcriptPath`. Do not run this against live production sessions without explicit approval.

## Prerequisites

- Local `agy` CLI installed and authenticated.
- Isolated `AO_DATA_DIR` (for example `/tmp/ao-agy-smoke-data`) and isolated `GEMINI_HOME` (or a scratch workspace) so live sessions are never mutated.

## Steps

1. Start an isolated AO daemon:
   ```bash
   AO_DATA_DIR=/tmp/ao-agy-smoke-data go run ./backend/cmd/ao start
   ```
2. Spawn a throwaway AGY TUI session in a scratch repository and send a single prompt (for example, `"Reply with: smoke-ok"`).
3. Wait for the turn to complete (`Stop` hook fires `ao hooks agy stop`).
4. Verify:
   - The session's `native_transcript_path` is populated and points inside `$GEMINI_HOME/antigravity-cli/brain/<conversation-id>/`.
   - The completed turn's user request and final assistant message appear in `session_terminal_turns` (`ListTerminalTurns`).
   - Re-sending the same `Stop` payload does not create a duplicate terminal turn.
   - After daemon/session recovery (`FirstSignalAt` reset), an idle session legitimately degrades to `no_signal` after 90s until the first fresh hook (`pre-invocation`, `post-tool-use`, or `stop`) restores activity health.

## Known Limitations

- AGY TUI does not emit a separate `UserPromptSubmit` hook; turn requests are paired from the latest `USER_INPUT` step preceding the final `PLANNER_RESPONSE` (`status: "DONE"`) in the validated transcript tail.
- Transcript reads are bounded to the last 4 MiB of regular `.jsonl` files under trusted `$GEMINI_HOME` AGY roots (`antigravity-cli`, `antigravity-acp`, `antigravity`). Symlinks escaping those roots, non-regular files, and cross-session brain paths are rejected.
- If a turn fails, is canceled, or ends without a completed text `PLANNER_RESPONSE`, no assistant response is recorded (AO never falls back to an earlier turn).
