# Agent runtime landscape

**Research snapshot:** 2026-11-28. Companion to [AO's long-term agent execution OS vision](../agent-os-vision.md). This is a comparative reading of public project documentation, not a source-code audit, feature certification, benchmark or decision to adopt any dependency. Recheck upstream revisions before relying on specific behavior.

## The distinction that matters

An agentic coordinator can choose what to do, decompose work and spawn subagents. A durable control plane owns admission, task identity, scheduling, evidence, recovery and authorization outside that coordinator's conversation. These are compatible layers, not mutually exclusive products. A project may combine both, and the boundary can evolve over time.

For AO, the question is whether already-authorized tasks can progress safely when the planning/orchestrator LLM is unavailable. Independent verification is an evidence/trust boundary, not an argument for mandatory human-role simulation.

## References

| Project | Publicly described approach | What AO should study |
| --- | --- | --- |
| [OpenAI Symphony](https://github.com/openai/symphony/blob/main/SPEC.md) | Long-running issue-driven scheduler, isolated workspaces, agent runs and reconciliation. The spec does not require a durable local orchestrator database. | Tracker reconciliation, scheduling contracts, repository-owned workflow configuration, retry and workspace lifecycle. |
| [Orkestra (andyyaro)](https://github.com/andyyaro/orkestra) | Agentic director with deterministic policy/verification kernel, worktrees and independent review. [Kernel design rule](https://github.com/andyyaro/orkestra/blob/main/CONTRIBUTING.md). | Director/kernel boundary, structured decisions, evidence-based acceptance, verification preflight. |
| [Orka (Dusttoo)](https://github.com/Dusttoo/orka) | Orchestrator session delegates ticket work to implementation and review roles; newer documented durable supervision separates logical jobs from dispatch/execution identity. [README](https://github.com/Dusttoo/orka/blob/main/README.md). | Compare delegation methodology with the newer supervisor and job recovery contracts; inspect worktree/PR evidence, cancellation and writer ownership before adopting ideas. |
| [Vigilante](https://github.com/aliengiraffe/vigilante/blob/main/README.md) | GitHub issue-to-PR control plane with managed worktrees, scoped execution and operator audit trail. | Credential and sandbox boundaries, lifecycle evidence, explicit operator intervention. |
| [Gas Town](https://github.com/gastownhall/gastown/blob/main/README.md) | Persistent Beads work records, agent coordinators and patrol roles. | Durable work identity and handoff, while measuring the cost of model-driven supervisory loops. See also [Gas City migration concepts](https://github.com/gastownhall/gascity/blob/main/docs/getting-started/coming-from-gastown.md) for first-class lifecycle mechanisms. |
| [AIOS](https://github.com/agiresearch/AIOS) / [paper](https://arxiv.org/abs/2403.16971) | Research-oriented LLM agent OS addressing scheduling, context, memory and tool resources. | OS-inspired abstractions and capability boundaries; not a direct substitute for AO's software-delivery runtime. |
| [Temporal](https://docs.temporal.io/durable-execution) | General-purpose durable workflow execution, persistent event history, replay and worker task queues. | Idempotent external effects, retries, durable timers, task queues and recovery semantics. See [event history](https://docs.temporal.io/encyclopedia/event-history) and [tasks](https://docs.temporal.io/tasks). This does not imply migrating AO to Temporal or replaying nondeterministic LLM reasoning. |

## Firsthand operator observation: Orka

The AO fork owner reports trying a project called Orka once. In that usage, an agent itself acted as the orchestrator and spawned subagents to perform work. The observed methodology was agent-led delegation rather than a standalone deterministic scheduler being the primary coordinator.

This is a qualitative observation of one run, not an assessment of current Orka releases, reliability, cost, or every execution mode. The exact repository, version and configuration of that run were not recorded in this discussion. Multiple projects use the name Orka (including [Dusttoo/orka](https://github.com/Dusttoo/orka), [Enophia-Studio/orka](https://github.com/Enophia-Studio/orka) and [orka-agents/orka](https://github.com/orka-agents/orka)); do not attribute this observation to a specific implementation until confirmed. The Dusttoo documentation independently describes an orchestrator-session delegation model as well as durable supervisor work; those public claims do not invalidate the firsthand observation or establish what was deployed in that run.

The lesson for AO is not that spawning subagents is undesirable. A worker or planning agent may delegate bounded research or implementation subtasks. The distinction is ownership: cross-task admission, durable task state, recovery, feedback delivery and authority must remain enforceable by AO even if the parent agent stops. Do not require an LLM to poll, remember, or reissue routine lifecycle decisions.

## Investigation priorities (not new implementation commitments)

- Orkestra: determine whether the director can be stopped after plan admission and what the kernel continues to execute without it.
- Symphony: inspect reconciliation, retry/workspace identity, dependency blocking and the restart semantics of its tracker-driven model.
- Dusttoo/orka: distinguish the agent-led entry point from its durable supervisor; inspect which states are persisted, recovery eligibility, writer fencing and whether coordinator absence prevents progress.
- Temporal: use durable execution concepts as a design reference for AO #73 recovery and future task mailboxes, not a new runtime requirement.
- Vigilante and AIOS: review least-authority mechanisms, process isolation and capability management.
- Gas Town/Gas City: compare model-driven patrols against deterministic event-driven wake/sleep and account for both model and host resource use.

## Comparison protocol

For future hands-on evaluations, record exact repository, commit/release, provider/harness, configuration, task scope, model effort and test environment. Separate documented, source-inspected and live-tested capabilities. Use disposable repositories and equivalent tasks, and test coordinator loss, worker loss, quota refusal, duplicate feedback, restart, stale CI/review evidence and dependency release. Record successful delivery, human interventions, token/quota measurements (separately), wall time, recovery and duplicate side effects. Do not infer production readiness or cost superiority from architecture descriptions alone.

Keep existing AO backlog as the delivery plan: [#16](https://github.com/wangyu0426/agent-orchestrator/issues/16), [#6](https://github.com/wangyu0426/agent-orchestrator/issues/6), [#42](https://github.com/wangyu0426/agent-orchestrator/issues/42), [#57](https://github.com/wangyu0426/agent-orchestrator/issues/57), [#58](https://github.com/wangyu0426/agent-orchestrator/issues/58), [#62](https://github.com/wangyu0426/agent-orchestrator/issues/62), [#63](https://github.com/wangyu0426/agent-orchestrator/issues/63), [#64](https://github.com/wangyu0426/agent-orchestrator/issues/64), [#68](https://github.com/wangyu0426/agent-orchestrator/issues/68), [#72](https://github.com/wangyu0426/agent-orchestrator/issues/72) and [#73](https://github.com/wangyu0426/agent-orchestrator/issues/73). Research findings should be mapped to existing contracts before proposing additional work.
