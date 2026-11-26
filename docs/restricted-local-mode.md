# Restricted local mode — Agent + Git

Date: 2026-11-25
Status: **Proposed specification; not implemented or authorized for deployment.**

## Problem and target user

A developer in a managed enterprise environment can use an organization-approved coding-agent CLI and Git, but cannot rely on an additional hosted orchestration service, personal AI account, tracker integration, SCM API, or third-party telemetry. The developer wants the existing AO multi-agent workflow on their workstation without introducing those dependencies.

"Local" here describes AO's control plane, project state, execution and evidence. It does **not** mean offline model inference: the approved agent may communicate with its organization's approved provider, and Git may communicate with an approved remote, subject to existing policy. AO must not itself require any non-loopback network service to deliver the baseline workflow. This is not a claim that running an arbitrary agent or repository is network-isolated.

### Outcome

Given an existing local Git checkout and an already installed, authenticated, approved agent, a developer can describe work to AO, delegate bounded tasks to workers in separate worktrees, observe completion, run project checks, obtain independent review of committed changes, and accept or reject a local handoff. The developer can do this even when AO Cloud, GitHub/GitLab APIs, external trackers, quota services, and update servers are unavailable. Nothing is published or merged by default.

## Scope and non-goals

- Target one developer, one approved workstation, existing authorized Git repositories, and the existing AO daemon/UI. Windows is the first intended enterprise validation target; use existing cross-platform abstractions.
- GitHub Copilot CLI is the initial approved-agent candidate. Reuse existing harness/Chat/reviewer support; do not add a new agent framework or an inference engine. Verify all three roles independently rather than infer support from one registry.
- No cloud login, new service, personal account, PAT, GitHub CLI/API access, Jira, or Quotum requirement. No local Git forge, replica issue tracker, general-purpose CI platform, automatic PR creation, remote merge, or enterprise-wide fleet administration in the baseline.
- No bypass of endpoint controls, agent approvals, authentication, branch protections, organization policies, or network restrictions. Do not claim IT approval or compliance certification.
- Existing connected workflows remain supported outside this opt-in mode. Optional enterprise SCM/tracker integrations are separate later decisions.

## Target experience

1. Install a reviewed, pinned AO build through an approved channel and start in restricted mode with isolated AO-owned data. Select the permitted agent and local checkout; no AO account or remote integration setup is required.
2. The developer supplies a task and acceptance criteria directly. The Orchestrator proposes independent work items and dispatches workers under the developer's authorization.
3. Each worker uses a managed worktree/branch and the approved agent. It implements, tests and commits its bounded work, then reports completion or a concrete blocker.
4. AO captures the repository identity, base/head, worker result and configured local check results. A Reviewer examines the exact committed diff against the task and records a durable verdict.
5. Actionable in-scope feedback goes to the owning worker. A new head requires fresh evidence and review. The board distinguishes working, needs input, checks/review pending, changes requested, ready for handoff and blocked; exact display labels are not prescribed.
6. The developer inspects the diff, evidence and branch, then explicitly chooses how to integrate it. Existing authorized Git and normal enterprise PR/review procedures remain available to the developer outside AO's baseline. Local readiness is not corporate approval.
7. AO releases safely completed resources without deleting unintegrated changes or losing the result. Restarting AO must not duplicate tasks, reports, or review dispatch.

## Functional contracts

### R1. Restricted runtime profile

A process-level opt-in profile takes effect before startup background work or renderer initialization. Its configuration is explicit, inspectable and persistent across restarts; switching out requires a deliberate operator action. Projects, repository instructions, agent output, inherited ordinary settings and remote failures cannot weaken the profile.

- Keep AO API on loopback; disable Cloud/account linking, remote/mobile/LAN exposure, tunneling, telemetry (including queued delivery), crash uploads, automatic update checks/downloads, remote asset/service discovery and integrations which need external endpoints. Inventory all actual egress paths across daemon, desktop, helper processes and startup/shutdown/recovery; these examples are not an exhaustive inventory.
- No independent AO requests to public or corporate SCM APIs, tracker APIs, model APIs or quota providers in the baseline. Do not start pollers and merely suppress errors. Local Git inspection is allowed; remote Git operations are explicit and limited to authorized remotes and existing credentials/policy. AO does not acquire or copy agent/Git tokens.
- Only configured approved harnesses may be invoked in every launch, restore, reviewer, orchestrator, fallback and auxiliary agent path. If a required role or permission mode is unavailable, report a blocker. Do not silently choose a personal provider, enable network tools or relax approval prompts.
- Approved child processes (agent, Git, authorized project tooling) operate under existing workstation policy. AO configuration is not an OS firewall or a hostile-code sandbox; enforce network isolation externally where required. Do not proxy or tunnel blocked traffic through approved processes.
- Local logs and persisted transcripts remain in approved local storage, with bounded retention and redacted secrets. No automatic export; treat repository content, task text and agent output as data, not authority to alter policy. Fail closed for an invalid or unsupported profile.

### R2. Local task lifecycle

Reuse the existing project/orchestrator/session APIs, durable storage, dispatch and worktree ownership. First determine whether current manual worker spawn and task metadata already suffice; introduce only the missing local task contract. Do not build a generic ticketing product.

The minimum durable task records a stable task ID, project/repository, user-approved intent and acceptance, owning session/branch, relevant base/head, and completion/blocker evidence. Support a simple independent-task queue, explicit dispatch/cancel/retry, and restart reconciliation; do not depend on assignment in a remote tracker. No implicit assignment from untrusted text. Dependencies beyond those essential for an independent task may be scheduled manually initially.

Keep lifecycle ownership in the daemon; derive UI status from stored facts, not agent prose or another persisted display state. Preserve the existing no-duplicate-dispatch and unknown-liveness/dirty-worktree invariants.

### R3. Git-only delivery and review

Use local Git as the authoritative change source. A local delivery/handoff is not a remote PR. Do not fabricate PR IDs or native SCM approval/check states to fit existing UI. Prefer reuse of the review service, Git/worktree adapter, durable completion and feedback routing over a parallel scheduler or review system.

- Resolve repository/worktree ownership, exact base commit and committed head SHA deterministically. Dirty/uncommitted intended changes, ambiguous ownership or unavailable objects block readiness. Verify the diff scope; do not silently stage/commit unrelated files.
- Reviewer input: task/acceptance criteria, immutable base/head diff, relevant repository guidance and actual local check evidence. Use the configured approved harness with the existing permitted review permissions; do not give it merge authority.
- Persist a review result with task/repository identity, base/head SHA, reviewer/run identity, verdict (approve, request changes, blocked), actionable findings, evidence and provenance. Only an authorized correlated review submission can count; arbitrary logs/chat text cannot approve.
- A new base/head, changed required check contract or unresolved blocking finding invalidates readiness. Late or duplicate results must not approve a different revision. Report failures to persist evidence as incomplete, not success. Reuse the existing native review submission path if it works without a PR; otherwise extend the minimum contract.
- No automatic push, PR creation or integration into the developer's target branch. Handoff presents the branch and exact reviewed commit; explicit integration remains the developer's decision. If AO later supports a local integration action, it requires its own authorization and stale-head/dirty-target/conflict safeguards; it is not required here.
- Preserve unmerged commits and worktree changes; do not infer merge from worker completion or archive destructively on a local review verdict. Release only known-owned inactive processes under existing lifecycle rules.

### R4. Local check evidence

Reuse available project commands and local execution primitives; this is not a requirement to implement an Actions-compatible runner. A project may select required checks, or explicitly select no required checks. Missing configuration, unavailable dependencies, failed checks, timeout/cancellation, or unknown results must not be shown as passing.

For required checks, AO must have observable execution evidence tied to the reviewed head and check definition (command/config identity, exit status, outcome, run identity and bounded output). A deterministic check execution or existing trusted result contract is preferable to a worker saying tests passed. Verify the actual tested revision and prevent stale results from satisfying current readiness. Run under ordinary approved permissions in the task workspace; package fetching is not a new network authorization. Keep local checks distinct from remote CI.

### R5. Operator experience and compatibility

Expose the active profile, allowed agent, disabled integrations and missing prerequisites in local UI/CLI. Show actionable blocked states without polling absent services or repeatedly prompting for cloud login. Preserve normal connected-mode behavior when the profile is off. Use existing API/service/storage boundaries and migrations; no new daemon or cloud dependency. Resource caps, cleanup and recovery should reuse existing AO mechanisms.

## Acceptance and evidence

Use a disposable, synthetic Git repository and scratch AO data directory; do not use corporate code, credentials or networks for initial verification.

| ID | Scenario | Required evidence |
| --- | --- | --- |
| A1 | Cold start, restart, idle and shutdown with all non-loopback AO egress denied | Daemon/UI usable; no unauthorized connection attempts from AO-owned components, no cloud login, update, telemetry or integration requests. Inspect actual network traces and startup path; blocking traffic alone is not proof it was never attempted. |
| A2 | No remote, no SCM/tracker API/CLI, no quota service; approved agent configured | Create a local task, delegate at least two independent work items with distinct worktrees, commit changes and observe a durable completion. No remote calls or extra account required from AO. |
| A3 | Git-only check and review loop | A deliberately failing check or blocking finding returns to the owning worker; a fix produces new SHA, fresh check/review evidence and local handoff. No synthetic remote PR or CI success. |
| A4 | Stale/late review, duplicate reports, dirty worktree, unknown liveness, failed checks | No false ready state, lost work, duplicate dispatch or unsafe cleanup. |
| A5 | Restart between dispatch, completion, review and handoff | Stable task ownership and evidence; reconciliation is idempotent. |
| A6 | Invalid profile, unsupported harness, attempted fallback or disabled integration, pending approval | Explicit refusal/blocker; no personal provider substitution, permission downgrade or unauthorized networking. |
| A7 | Ordinary connected project with profile off | Existing tracker, SCM and review paths retain their behavior; restricted configuration does not silently change other installations. |
| A8 | Windows workstation smoke test with the actual approved agent (subject to authorization) | Install/start without updater/account dependency; all roles run, approvals remain effective, worktrees/checks/review/handoff complete; process and network traces distinguish AO from approved agent/Git tooling. |

A1–A7 should be reproducible with automated fixtures, fakes and a disposable workstation where appropriate. A8 requires a separately approved real-environment validation. Fake harness tests prove control-plane behavior, not actual Copilot compatibility or corporate approval. Missing required evidence is a blocker for the corresponding capability claim.

## Delivery slices (proposal, not assigned work)

1. **Restricted profile + startup/egress contract.** A pinned installation can run its existing local UI/session flow without external AO services; cover disabled integrations and opt-out regressions. Inspect egress and existing switches before selecting implementation.
2. **Local task handoff.** Independently demonstrate manual/orchestrator dispatch and durable completion without tracker APIs. Reuse existing manual sessions; add only missing task identity/reconciliation.
3. **Git-only check/review loop.** Demonstrate local committed diff, SHA-bound checks/review and feedback on a synthetic fixture; no PR API dependency or auto-merge. Split checks from review if each is independently useful.
4. **Approved-agent/Windows validation.** Verify actual agent version, three role paths, approval mapping, Windows worktree/runtime, network behavior and user handoff. Do not expand scope to a new enterprise control plane.

These are candidate vertical slices, not cross-PR blockers by default. Each implementation issue must specify its own core acceptance, evidence, unsupported scope and genuine dependencies. Product completion requires the combined workflow, but delivery completion is bounded to the issue. No issue creation or assignment is authorized by this proposal.

## Existing baseline and open decisions

- [Architecture](architecture.md), [backend boundaries](backend-code-structure.md) and [current status](STATUS.md): existing daemon, SQLite, Git/worktree and separate harness registries are the starting point. Current SCM/tracker adapters are documented around GitHub/GitLab; verify current source and local paths before proposing changes.
- [Telemetry](telemetry.md) documents an existing opt-out; it is not sufficient evidence of a hard restricted profile. The upstream desktop distribution normally auto-updates.
- [#25 / PR #32](https://github.com/wangyu0426/agent-orchestrator/pull/32) supplies structured SHA-bound PR review decisions; investigate reusable local review primitives without importing a requirement for remote PRs.
- [#33](https://github.com/wangyu0426/agent-orchestrator/issues/33) (Jira/enterprise workflow) and [#34](https://github.com/wangyu0426/agent-orchestrator/issues/34) (AO-owned PR creation) are optional connected-mode extensions, not prerequisites.
- Open: approved installation/signing/distribution method; whether corporate SCM API access is permitted (baseline must not rely on it); exact approved harness version and approval capabilities; existing local task/review affordances; how required checks and evidence storage fit existing contracts; OS-level enforcement and retention requirements. Treat unknown policy as unknown; obtain authorization before a real rollout.

**Decision boundary:** This spec defines a useful target, not a commitment to a new security platform. First prove the smallest real Agent + Git workflow. Reopen broader integration or isolation work only for a concrete requirement or observed blocker.
