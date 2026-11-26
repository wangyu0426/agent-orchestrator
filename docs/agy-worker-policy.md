# AGY Worker Approval and Sandbox Policy

This document describes how Agent Orchestrator (AO) configures approval and sandbox policies for AGY (Antigravity) worker sessions across TUI and native ACP (`agy acp`) transports.

## Orthogonal Policy Dimensions

AO models three independent dimensions for worker sessions:

1. **Interaction transport** (`SessionMode`): `tui` or `chat` (native `agy acp`).
2. **Approval policy** (`PermissionMode`):
   - `default`: defer to the agent's own baseline approval settings.
   - `accept-edits`: auto-approve file edits (`agy --mode accept-edits` / ACP `accept-edits`).
   - `auto`: AGY's `--mode auto` (note: in `agy` 1.5.x, `--mode auto` still prompts interactively before shell commands that leave the terminal sandbox or are outside its allowlist).
   - `bypass-permissions`: auto-approve tool permission requests without prompting (`agy --dangerously-skip-permissions` / ACP allow-once policy).
3. **Sandbox / access policy** (`SandboxMode`):
   - `default`: defer to the adapter/agent baseline (does not silently disable sandboxing or choose unrestricted access).
   - `restricted`: run inside AGY's terminal sandbox (`agy --sandbox` / `agy acp --sandbox`).
   - `unrestricted`: explicitly opt out of `--sandbox` (operator-authorized only).

## Recommended Unattended Worker Policy

For unattended AGY workers on new workspaces/worktrees, prefer **automatic approval within a restricted sandbox**:

- `permissions`: `bypass-permissions`
- `sandbox`: `restricted`

This launches:
- **TUI**: `agy --add-dir <workspace> --sandbox --dangerously-skip-permissions [--model <model>] [--prompt-interactive <prompt>]`
- **TUI restore**: `agy --add-dir <workspace> --sandbox --dangerously-skip-permissions [--model <model>] --conversation <id>`
- **Native ACP**: `agy acp --sandbox` (with AO's ACP permission policy approving requests once)

## Configuration and Precedence

Policies resolve deterministically in the following order (highest precedence first):

1. Per-spawn / per-session override (`AgentConfig.Permissions` / `AgentConfig.Sandbox`)
2. Project role override (`ProjectConfig.Worker.AgentConfig`)
3. Project base config (`ProjectConfig.AgentConfig`)
4. Daemon-wide worker defaults:
   - `AO_DEFAULT_WORKER_PERMISSIONS` (`default`, `accept-edits`, `auto`, `bypass-permissions`)
   - `AO_DEFAULT_WORKER_SANDBOX` (`default`, `restricted`, `unrestricted`)
5. Built-in defaults: `permissions = auto`, `sandbox = default`.

Resolved `permissions` and `sandbox` are persisted in `SessionMetadata` at spawn so later resume/restore calls preserve the exact policy even if project or global defaults change.

## Compatibility and Fail-Closed Behavior

- **Unsupported sandbox mode**: Both `agy` TUI and `agy acp` fail closed with an actionable error rather than silently ignoring an unrecognized sandbox policy.
- **Legacy `agy_acp_server`**: Requesting `sandbox: restricted` on the legacy separate `agy_acp_server` fails closed with `ErrChatDriverIncompatible` because `--sandbox` requires native `agy acp` (`agy >= 1.5.4`).
- **Reviewer isolation**: Read-only reviewer sessions (`ReadOnly: true`) always pin `--sandbox`, `default` mode, and the strict read-only reviewer permission policy regardless of worker `permissions` or `sandbox` settings. `ProjectConfig.DefaultWorkerReviewer()` never inherits worker `permissions` or `sandbox`.

## Security Limits and Manual Recovery

- **Worktree is not a security boundary**: A Git worktree isolates branch state, not host filesystem or process access. `agy --sandbox` enables AGY's terminal restrictions, which is narrower than unsandboxed host execution but is not full OS container/VM isolation.
- **Manual recovery**: If an interactive AGY TUI worker stops at a `"Run this command?"` or folder-trust prompt (for example under `permissions: auto` or `default`), AO's `TerminalSurfaceInspector` surfaces `TerminalSurfaceWorkWaitingInput` without sending blind keystrokes. Operators can inspect the terminal directly or relaunch with `AO_DEFAULT_WORKER_PERMISSIONS=bypass-permissions` and `AO_DEFAULT_WORKER_SANDBOX=restricted` (or per-project `agentConfig`).
