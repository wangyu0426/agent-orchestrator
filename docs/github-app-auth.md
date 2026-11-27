# GitHub App Authentication for AO Daemon

The AO daemon supports an opt-in **GitHub App installation** authentication mode for its SCM and Tracker adapters. This gives the daemon an independent bot identity (`<app-slug>[bot]`) separate from the operator's personal GitHub account, without changing worker `git` or `gh` credentials.

## Configuration

GitHub App mode is **disabled by default**, preserving the existing `AO_GITHUB_TOKEN` → `GITHUB_TOKEN` → `gh auth token` credential chain.

To enable App mode, set the following environment variables when starting the daemon:

| Variable | Required | Description |
| --- | --- | --- |
| `AO_GITHUB_APP_ENABLED` | Yes | Set to `on` / `true` / `1` to enable GitHub App mode. |
| `AO_GITHUB_APP_ID` | Yes (when enabled) | Numeric GitHub App ID. |
| `AO_GITHUB_APP_INSTALLATION_ID` | Yes (when enabled) | Numeric installation ID for the target org/account. |
| `AO_GITHUB_APP_PRIVATE_KEY_PATH` | Yes (when enabled) | Path to the PEM-encoded RSA private key file (PKCS#1 or PKCS#8). |
| `AO_GITHUB_APP_REPOSITORIES` | No | Optional comma-separated list of repository names (or `owner/repo`) to scope issued installation tokens. When omitted, tokens inherit the installation's granted repositories. |
| `AO_GITHUB_APP_API_BASE_URL` | No | Override GitHub REST API base URL (defaults to `https://api.github.com`). |
| `AO_GITHUB_APP_OPERATOR_LOGIN` | No | Optional GitHub username preserved for identity-only PR claim and automatic PR-to-session attribution checks while all GitHub API requests still use the App installation token. |

## Fail-Closed Behavior

When `AO_GITHUB_APP_ENABLED` is enabled:

- Startup fails immediately if `AO_GITHUB_APP_ID`, `AO_GITHUB_APP_INSTALLATION_ID`, or `AO_GITHUB_APP_PRIVATE_KEY_PATH` is missing, unreadable, or not a valid RSA private key.
- Daemon SCM, Tracker, PR merger, and repository resolver paths use **only** the GitHub App installation token source.
- The daemon **never** silently falls back to `AO_GITHUB_TOKEN`, `GITHUB_TOKEN`, or `gh auth token`.

## Security & Isolation Limitations

- **Private key hygiene**: Never commit the `.pem` private key to any repository. Store it with `chmod 600` outside the workspace.
- **Worker environment scrubbing**: After `config.Load()`, the daemon calls `config.ScrubGitHubAppEnv()` to unset all `AO_GITHUB_APP_*` variables from `os.Environ()` so spawned workers do not inherit the key path or app metadata.
- **Secret redaction**: Error messages and `String()`/`GoString()` outputs redact PEM blocks, JWTs, and `ghs_`/`ghp_`/`github_pat_` tokens.
- **Same-UID limitation**: On Linux/macOS, workers running under the **same OS UID** as the AO daemon share filesystem permissions with the daemon user. Scrubbing environment variables prevents accidental leakage, not a malicious same-UID process from reading a user-readable file. Stronger isolation requires running the daemon under a dedicated service user or separate credential boundary.

## Deployment & Rollback

Deployment to production hosts must be operator-approved.

### Rollback to Legacy PAT / `gh` Mode

1. Unset `AO_GITHUB_APP_ENABLED` (or set `AO_GITHUB_APP_ENABLED=off`).
2. Restart the AO daemon.
3. Verify legacy `AO_GITHUB_TOKEN` / `GITHUB_TOKEN` / `gh auth token` operation.
