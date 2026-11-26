# AO fork UI parity — design specification

**Date:** 2026-11-26 (Australia/Sydney)  
**Status:** Proposal for owner review; no implementation or deployment authorized.  
**Target:** wangyu0426/agent-orchestrator, Go daemon + Electron/React supervisor, including the self-hosted web client where the same renderer is supported.

## 1. Goal and motivation

Make fork-specific capabilities discoverable and operable through the existing AO UI, rather than requiring SSH, shell commands, environment-file inspection, or log digging for routine administration. The problem is feature parity between our daemon/CLI and our UI, not dissatisfaction with the upstream visual design.

On 2026-11-26 the operator's yuworker reported 39 merged Worker candidates: 3 eligible, 36 deferred (24 runtime ownership unknown, 12 Chat ownership unknown). After manual termination the merged-candidate preview was empty, but /tmp still occupied 22 GiB of a 31 GiB tmpfs; task-cache cleanup reduced that to 5.1 GiB, and available memory increased from 32 to 48 GiB. This is evidence of a real operator workflow gap, not evidence that all deferred processes were live, or that the proposed GC would collect legacy paths.

Success: an operator can understand effective policy, runtime decisions and maintenance eligibility, and perform supported, explicitly authorized actions without leaving the UI. CLI remains a supported equivalent.

## 2. Scope and non-goals

- Extend the current upstream-derived UI with additive fork modules; preserve board, session list, terminal/chat, project settings, navigation and existing interaction conventions.
- Reuse existing typed daemon API and authoritative services; add narrow endpoints/read models only where needed. No second orchestrator, duplicate scheduler, UI-side policy evaluator, direct SQLite or process/filesystem control from the renderer.
- Do not rewrite the UI, fork a second frontend, add a component framework, or make broad changes to upstream-owned components.
- Do not implement resource admission, ownership reconciliation, temp GC, dependency workflows, auto-merge or new provider features as part of UI work; their backend issues remain independent. No force kill, arbitrary /tmp cleanup, automatic deployment, or changes to auth boundaries.
- Initial scope is self-hosted desktop and the existing supported web renderer. No new mobile app, Cloud control plane or remote access mechanism. Verify shared renderer compatibility where applicable; unsupported capabilities must be explicit.

## 3. Capability inventory and evidence gate (UI-0)

Initial audit from merged PR changed-file lists; absence of frontend changes is not proof a feature is invisible through an existing generic UI. Audit current main, actual generated API, renderer routes, and real app screens before implementation; record exact SHA and evidence for each row.

| Capability | Backend evidence | Initial UI finding / target |
| --- | --- | --- |
| Archive-on-merge settings | #5 / #24 merged | Existing global/project/session settings: retain and verify effective inheritance |
| Bulk archive merged | #24 merged | CLI + API exist; expose preview/apply and per-session deferral reasons |
| Worker quota policy | #14 / #21, #26 / #29 merged | Show configured/effective routes, actual choices and failures |
| Reviewer and Orchestrator quota policies | #28 / #35 merged | Separate role policies and decisions, no implicit shared route |
| Reviewer quota diagnostics | #45 / #46 merged | Surface structured rejection reasons and evidence freshness; inspect available API |
| AGY ACP and TUI, auth/readiness | #37 / #38, #40 / #41, #39 / #44 merged; #47 open | Show actual transport, readiness and failure; do not claim E2E fallback solved |
| PR discovery and AGY turn reports | #18 / #30, #20 / #31 merged | Verify existing session UI reflects durable association and report facts; add only missing diagnostics |
| Structured review-body decisions | #25 / #32 merged | Show supported structured review evidence and exact HEAD; audit generic UI first |
| Session-owned temp GC | #15 / #22 open, not merged at baseline | Proposed UI only; gate on shipped API and semantics |
| Concurrency/resource admission | #16 open | Future capability; no invented controls |
| Historical process ownership recovery | Not established as shipped | Show current deferral; separate backend follow-up, not bypass |

Evidence sources: PRs and current daemon source; UI-0 must distinguish shipped, unmerged, planned, unsupported and unknown. Recheck upstream on each sync, record overlap before adding code, and reuse upstream equivalents where available.

## 4. Interaction architecture

Three user-facing surfaces, final placement subject to UI-0's navigation audit:

1. **Routing & Quota** (settings/operations): role-level policy inspection and, only where backed by a validated write contract, editing.
2. **Maintenance & Resources** (host-level operations): archive preview/apply, temp GC when available, host/resource evidence.
3. **Session Diagnostics** (existing session detail/inspector): context for actual runtime, review and blocking reasons.

Keep global/host data global; project filtering is explicit, not an implicit change in configuration. Reuse existing settings patterns, tables, status and confirmation primitives. Use typed client query/mutation hooks, existing caching and invalidation/CDC mechanisms; do not introduce a parallel event bus. Prefer a small isolated module and minimal existing-shell integration points; keep generated API changes code-first. All policy decisions, authority checks and persistence remain in the daemon. UI renders server facts, not derived persistent status.

### 4.1 Routing & Quota

For each role (Worker, Reviewer, Orchestrator), show enabled state, configured source (environment/project/default as supported), ordered primary/fallback entries, harness, model, effort, mode and opaque account/source binding where safe. Distinguish policy from actual active runtime selection; manual overrides must be visible and not silently overwritten.

Show quota window, available/exhausted/unknown/stale/unmapped states, observation timestamp and source, and selected route or deferral. Present structured reasons where available: quota exhausted, stale evidence, auth/setup required, unsupported model/mode, readiness failure, retry cap, unknown. Do not infer readiness from a binary or OAuth file; do not expose raw tokens, full credentials, prompts, or entire environment variables.

MVP is read-only for effective env-owned configuration, with copyable sanitized diagnostics and documentation on changing it. Do not build a UI editor that writes systemd drop-ins, shell files or arbitrary env vars. A later write-capable slice requires explicit daemon-owned persistence, validation, precedence rules, concurrency/version checks, preview of effective diff, clear activation semantics (new launches vs current sessions), owner confirmation and rollback. Never silently restart daemon or switch active turns. Preserve existing controls and policies.

### 4.2 Maintenance & Resources

**Archive merged Workers (first vertical slice):** use the existing #24 preview/apply API and its authoritative eligibility. Show scope, snapshot time, candidate ID/title/project, PR identity where available, eligible/deferred reason, counts, and explicit selection of eligible candidates. Preview performs no teardown. Confirmation names exact selected IDs/count and scope; apply only those IDs from that preview, daemon rechecks all safety gates. Show per-ID success, skip, failure and reason; refresh after completion, keep partial results. Never silently expand selection to newly eligible sessions. A stale preview may safely fail or skip; no automatic force fallback. Unknown ownership is not proof of exit. Do not conflate archive/terminate, workspace cleanup and permanent delete. Preserve history.

**Temp GC:** only after #22 or successor merges with a stable supported contract. Display AO-owned root, configured retention, eligible/deferred counts, reason and estimated bytes when provided. Dry-run, explicit confirmation, exact preview scope where supported, daemon recheck and result bytes/errors. If the backend currently only supports bulk re-evaluation, expose that accurately or add a narrowly scoped contract before allowing selective actions. No UI traversal/deletion of host-wide /tmp, shared caches or legacy directories. Do not promise that TMPDIR covers explicitly hardcoded paths. Distinguish archive release from delayed retention-based GC.

**Host resources:** initially read-only, with bounded daemon-side sampling and platform availability. Show total/available memory, swap, temp-root filesystem type/usage, optional /tmp usage and AO-owned process counts where authoritative. Label measurement time, units and scope. RSS is not unique memory; tmpfs/shmem may overlap OS counters. Distinguish host-wide from AO-owned metrics and unknown ownership. No expensive unbounded directory/process scans from frontend; no raw process args, environment, or unrelated host data. A new metrics endpoint requires review of permissions and platform semantics. Concurrency controls depend on #16; don't invent admission values before implementation.

### 4.3 Session Diagnostics

Expose current harness/model/effort/mode, provider/transport, actual runtime vs configured route, safe liveness/ownership evidence, activity, handoff and PR association status from daemon facts. Show last decision/error, timestamps and supported recovery action; distinguish idle, exited, parked, terminated and unknown. Structured review verdict must be bound to the reviewed HEAD; stale approval is not current approval. Separate reviewer runtime, review result, and PR CI state. Provide redacted diagnostic copy and links to existing session/PR context. Do not duplicate full logs or let diagnostics override safety checks.

## 5. API, security and failure behavior

- Maintain current daemon/renderer/CLI boundary and generated OpenAPI client. Inspect existing API route contracts before choosing new names. Do not hand-edit generated schema; regenerate via repository commands.
- Local/remote access follows AGENTS.md: loopback stays loopback; opt-in LAN access retains existing auth and host identity rules. No new listener, auth exemption, privilege escalation or direct renderer shell. Explicitly decide local-only versus authorized remote availability for every maintenance endpoint; default to local-only for newly privileged controls until reviewed. Verify existing endpoint exposure rather than assuming it is safe because UI hides a button.
- All destructive operations require explicit user confirmation, bounded scope, server-side validation, idempotent/reconcilable results and exact session ownership checks. Preserve current daemon safety and existing permission semantics.
- Unavailable endpoints/older daemon versions render unsupported with a useful explanation, never fake zero, green health or empty state. Auth error, timeout, stale data, partial result, concurrent session transition and unknown state get distinct UI treatment. Do not optimistically mark terminated or freed until authoritative response/reconciliation.
- A lost response is uncertain; refresh state before retrying. Do not expose secrets or raw provider responses in diagnostics. Use existing request/error handling and logging conventions.
- No policy defaults change solely because a UI is introduced.

## 6. Independent delivery slices (proposed, not assigned)

| Slice | Deliverable | Dependencies / acceptance |
| --- | --- | --- |
| UI-0 | Capability matrix and existing UI/API audit, screenshots of actual desktop/shared web surfaces, minimal navigation plan | Read-only; record SHA, upstream overlap and real missing capabilities |
| UI-1 | Archive merged preview/apply UI | #24 shipped; first useful independent vertical slice |
| UI-2 | Read-only role routing and quota diagnostics | #21/#29/#35/#46; add narrow read models only if required |
| UI-3 | Session runtime and review diagnostics | Existing runtime, SCM and review facts; #47 fixes remain independent |
| UI-4 | Temp GC UI | #22 merged and contract verified; independent of routing |
| UI-5 | Read-only host/resource telemetry | Bounded sampling, privacy/platform contract; no process control |
| UI-6 | Optional route configuration editing and resource-admission controls | Separate approved write spec; #16 for admission; no automatic rollout |

Parallelize genuinely independent slices after audit, not changes that fight over shared navigation or API contracts. Each slice gets a bounded issue/PR, independent tests and review. Do not block a working archive UI on future routing/GC implementations. No automatic merging or deployment.

## 7. Testable acceptance

- UI-0 records every listed capability with implementation status, API source, actual screen/entry or gap, and plan for upstream overlap. No claims of missing UI solely from changed-file counts.
- UI-1 fixture reproduces the observed 3 eligible / 36 deferred result (24 runtime ownership unknown, 12 Chat ownership unknown). Only explicitly selected eligible IDs are sent on apply; deferred reasons visible; tests cover empty list, preview cancellation, stale/changed candidates, partial failures, duplicate submit, auth/transport failure and refresh after uncertain result. A changed or newly eligible ID is never silently added. CLI and UI call the same service.
- Routing tests cover role separation, configured vs effective route, stale/unknown quota, exhausted primary with eligible fallback, all unavailable and explicit override. No client-side route choice or credential exposure. Unsupported write actions are absent/disabled with explanation.
- GC tests verify retention/eligibility/result display, unsupported backend, preview/confirm semantics, unknown ownership and no arbitrary path deletion. UI never weakens backend policy.
- Session tests cover stale HEAD verdict, provider handoff, unknown process ownership, missing evidence, error/recovery and distinction between terminated vs exited.
- Metrics tests cover unavailable platform, stale data, host/AO scope and bounded sampling; memory counters are not misleadingly summed.
- Use repository frontend tests, typecheck, relevant API/daemon tests and generated-contract checks. Verify real app UI in isolated scratch AO_DATA_DIR/worktree per AGENTS.md, using synthetic fixtures only; capture before/after and loading/error states, keyboard/focus, responsive layout and existing navigation regressions. Do not access or modify production data for tests. Report any unavailable environment separately.
- Upstream sync review checks that changes stay additive and document any necessary modifications to shared components; no visual rewrite. Owner acceptance/merge and deployment are separate.

## 8. Open decisions

- Final navigation placement and existing equivalents (UI-0).
- Whether effective policy and structured route decisions already have suitable bounded read endpoints; any missing fields require service contract review.
- Whether and how authenticated remote users may execute specific maintenance actions; no new exposure assumed.
- Whether config editing is worth adding vs read-only settings plus documented env management; defer until read-only UX proves need.
- Host telemetry platform support, refresh cadence and resource-admission semantics.

Owner decision requested: approve the direction and slice UI-0 first, then UI-1. This spec is a proposal, not authorization to spawn workers, change live config or ship features.
