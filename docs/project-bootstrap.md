# One-Command Project Bootstrap & Global Configuration Inheritance

This guide documents how to onboard a new Git repository to a self-hosted Agent Orchestrator daemon with a single command (`ao project add <repo-url>`), how daemon-wide defaults are inherited, and how to safely retry or troubleshoot onboarding.

## One-Command CLI Usage

```bash
ao project add <repo-url>
```

Example:

```bash
ao project add https://github.com/owner/new-service.git
```

Common options:

- `--destination <dir>`: override the daemon's `AO_PROJECTS_ROOT` parent directory for this clone.
- `--id <project-id>` / `--name <display-name>`: override the derived project ID or display name.
- `--branch <branch>`: pin a base branch instead of inferring the repository's Git default branch.
- `--worker-agent <harness>` / `--orchestrator-agent <harness>`: per-project role overrides.
- `--reviewer <harness>`: repeatable per-project reviewer override.
- `--auto-review` / `--no-auto-review`: explicitly enable or disable automatic PR review for this project (overrides global default).
- `--tracker-intake` / `--no-tracker-intake`: explicitly enable or disable tracker intake for this project (overrides global default).
- `--tracker-assignee <login>`: override the tracker intake assignee rule.
- `--json`: emit the full `BootstrapProjectResult` JSON payload.

Existing local-path registration continues to work unchanged:

```bash
ao project add --path /abs/path/to/local/repo
```

## Daemon Global Configuration

Configure daemon-wide defaults via environment variables on the daemon process. Secrets, GitHub tokens, and Quotum account bindings remain daemon-wide and are **never** copied into per-project config.

| Environment Variable | Purpose |
| --- | --- |
| `AO_PROJECTS_ROOT` | Approved absolute parent directory where `ao project add <repo-url>` clones repositories by default. |
| `AO_DEFAULT_WORKER_AGENT` | Default worker harness when neither project config nor `AO_WORKER_QUOTA_PRIMARY` sets one. |
| `AO_DEFAULT_ORCHESTRATOR_AGENT` | Default orchestrator harness when neither project config nor `AO_ORCHESTRATOR_QUOTA_PRIMARY` sets one. |
| `AO_DEFAULT_REVIEWERS` | Comma-separated default reviewer harnesses (e.g. `claude-code,agy`). |
| `AO_DEFAULT_AUTO_REVIEW` | `on` / `off` — daemon-wide default for automatic PR review on new worker sessions. |
| `AO_DEFAULT_TRACKER_INTAKE_ENABLED` | `on` / `off` — daemon-wide default for per-project tracker intake (still gated by `AO_TRACKER_INTAKE=on`). |
| `AO_DEFAULT_TRACKER_INTAKE_PROVIDER` | Default tracker provider (`github` or `gitlab`) when unset on a project. |
| `AO_DEFAULT_TRACKER_INTAKE_ASSIGNEE` | Default tracker intake assignee rule (e.g. `wangyu0426` or `*`). |

Existing daemon-wide quota routing (`AO_WORKER_QUOTA_*`, `AO_REVIEWER_QUOTA_*`, `AO_ORCHESTRATOR_QUOTA_*`, `AO_QUOTUM_*`) and the tracker intake kill switch (`AO_TRACKER_INTAKE`) continue to apply automatically to every project.

## Configuration Precedence

For each setting, AO distinguishes **unset** (inherit), **explicitly disabled**, and **explicitly configured**:

1. **Mandatory daemon gates & active quota primary** (cannot be bypassed by project config):
   - `AO_TRACKER_INTAKE=off` keeps tracker intake disabled daemon-wide even if a project enables it.
   - Daemon-wide quota failover and Quotum bindings remain owned by the daemon; when `AO_REVIEWER_QUOTA_PRIMARY` is configured with reviewer quota failover, it takes precedence over per-project reviewer selection (`source: "quota_primary"`).
2. **Explicit project override** (`source: "project"`):
   - `autoReview: true` or `autoReviewOverride: false` (`--auto-review` / `--no-auto-review`)
   - `trackerIntake.enabled: true` or `trackerIntake.disabled: true` (`--tracker-intake` / `--no-tracker-intake`)
   - `worker.agent`, `orchestrator.agent`, `reviewers` (when no reviewer quota primary overrides it), `defaultBranch`
3. **Daemon quota primary** (`source: "quota_primary"`, for worker/orchestrator harnesses when project role is unset):
   - `AO_WORKER_QUOTA_PRIMARY`, `AO_ORCHESTRATOR_QUOTA_PRIMARY`
4. **Daemon global defaults** (`source: "global"`):
   - `AO_DEFAULT_*` environment variables
5. **Built-in fallback** (`source: "default"` / `"git_remote"` / `"worker_inherited"`):
   - Git remote `HEAD` for default branch; `AO_AGENT` / `claude-code` for fallback harness.

## Safe Retry & Troubleshooting

`POST /api/v1/projects/bootstrap` (and `ao project add <repo-url>`) is idempotent and concurrency-safe:

- **Already registered at destination**: returns `action: "reused_project"` with HTTP `200` and no duplicate records.
- **Clean existing checkout after a partial failure**: verifies `remote.origin.url` matches and `git status --porcelain` is empty, then registers it (`action: "reused_checkout_and_registered"`).
- **Dirty existing checkout**: refuses with `CHECKOUT_DIRTY` (HTTP `409`) without modifying or deleting any local files. Clean/commit changes or pass `--destination`.
- **Empty / uninitialized remote repository**: cleans up staging directory and returns `CLONE_EMPTY_REPOSITORY`. Push an initial commit and retry.
- **Missing workspace root**: returns `BOOTSTRAP_WORKSPACE_ROOT_UNSET`. Set `AO_PROJECTS_ROOT` on the daemon or pass `--destination`.
- **Readiness blockers**: inspect `readiness.blockers` in the output (e.g. `AO_TRACKER_INTAKE` off, missing GitHub/GitLab credentials, or missing assignee rule).

## Bounded Opt-In Smoke Procedure (New Authorized Repo)

To verify end-to-end onboarding on a newly created private repository (for example, `wangyu0426/dev-rag-service`) against a running self-hosted daemon:

1. Ensure the daemon has `AO_PROJECTS_ROOT`, `AO_TRACKER_INTAKE=on`, `AO_DEFAULT_TRACKER_INTAKE_ENABLED=on`, `AO_DEFAULT_TRACKER_INTAKE_ASSIGNEE=<your-login>`, and `AO_DEFAULT_AUTO_REVIEW=on` configured, plus valid GitHub credentials.
2. Run:
   ```bash
   ao project add https://github.com/<owner>/<repo>.git
   ```
3. Confirm the output reports:
   - `action` is `cloned_and_registered` (or `reused_project` on re-run)
   - `branch` matches the repository's default branch
   - `auto-review: true (global)` and `tracker: enabled=true daemon-gate=true ... (global)`
   - `readiness: ready=true` with no blockers
4. Re-run the exact same command and verify `reused_project` is returned with no duplicate project in `ao project ls`.
5. Assign a small test issue in the repository to `<your-login>` and verify the daemon's tracker intake picks it up through the normal flow.
