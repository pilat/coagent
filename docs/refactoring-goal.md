# Core refactoring goal

## Purpose

Make the core understandable and safe to change by giving each runtime
invariant one explicit owner. Reduce the unrelated responsibilities concentrated
in daemon and session objects, eliminate competing implementations of the same
protocol, and make dependencies and resource lifetimes visible in construction.
Preserve the existing observable behavior and durable recovery guarantees.

This is the durable direction across implementation phases, not a claim that
the current architecture already meets it. Completing one phase does not
complete this goal. Smaller files and lower method counts are diagnostic
evidence, not success criteria.

## Desired result

- **Daemon coordinates cohesive capabilities.** Domain components own the
  state and operations that enforce their invariants. Daemon does not complete
  their transitions by manipulating their internals. An extraction must remove
  responsibility from its former owner, not hide it behind forwarding methods.
- **Session responsibilities can change independently.** The agent loop,
  transcript/context management, model configuration and tool execution have
  explicit boundaries. They do not require unrelated mutable session state to
  perform their work. Exact component boundaries must follow inspected code.
- **Persistence exposes complete atomic operations.** Consumers receive the
  capabilities they need; cross-table invariants stay inside their existing
  transactions. A large SQL implementation alone is not evidence that each
  table needs a separate repository.
- **Live execution and recovery agree.** They share protocol meaning and
  authoritative durable facts. Tests exercise production transitions; missing
  dependencies never select a second, weaker execution algorithm.
- **Control and presentation are independent.** Progress rendering and manager
  transport do not own execution policy. Their failure must not prevent budget
  enforcement, stop or recovery.
- **Ownership is traceable.** For every worker, resource, queue and durable
  transition, a reader can identify who starts, mutates, cancels, joins and
  recovers it without reconstructing implicit wiring across the application.

## Constraints

Keep the modular monolith, explicit composition and existing dependency tiers.
Prefer cohesive private types and existing packages before adding packages.
Do not introduce a generic workflow engine, actor framework, DI framework or
new event bus to distribute the same complexity.

Preserve accepted-input durability, stop/admission fences, resource shutdown
ordering, child-round identity, atomic completion delivery, response accounting,
candidate confirmation and at-least-once manager delivery. Preserve temporal
test scenarios when their mechanics change. The detailed contracts remain in
[ARCHITECTURE.md](../ARCHITECTURE.md), [the glossary](glossary.md), and
[the testing strategy](testing.md).

Product behavior, provider protocols, sandbox policy and schema redesign are
outside this effort unless separately agreed. Do not bundle performance work,
renaming sweeps or manager UI changes into ownership refactoring. Telegram
decomposition is a separate workstream; it is not required to finish the core.

## Completion criteria

The core goal is complete only when all of the following have evidence:

1. A review of the remaining daemon, session, lifecycle and persistence
   responsibilities finds no unresolved concentration of unrelated mutable
   state that makes routine changes cross those responsibilities. Necessary
   coordination is documented with its reason for remaining together.
2. Representative flows can be traced through named owners: input to response,
   child completion to parent delivery, stop and interrupted-stop recovery,
   budget crossing, compaction, and resource shutdown. Each invariant has one
   authority and explicit ordering; callbacks do not conceal another owner.
3. Each extraction has a concrete before/after improvement in dependencies,
   state access or duplicated protocol decisions. Introducing additional types
   without reducing those obligations does not satisfy the goal.
4. Behavioral and temporal checks pass, architecture documentation matches the
   code, and independent code review has no unresolved consequential findings.
5. Remaining debt is listed with a reason to defer it. There is no open-ended
   requirement to remove every large type or reach an arbitrary size threshold.

## Current checkpoint

- **Goal status:** complete. The ownership audit, independent reviews, full
  local tests, canonical CI and final local gate pass.
- **Completed implementation:** phase 1, runtime ownership, commit `a231abc`
  on `refactor/runtime-ownership`. See the
  [phase plan](../plan-2026-09-27-runtime-ownership.md) and
  [ADR-0065](adr/0065-runtime-coordination-owns-complete-transitions.md).
- **Established boundaries:** lifecycle supervision and tree-stop sequencing;
  explicit process/resource construction; one accepted-response path and
  recovered-outcome projection; budget reconciliation independent of progress.
- **Phase-1 validation:** plan validation and implementation-to-plan review
  passed; full `make ci` and final `make all` passed before the phase-1 commit.
- **Implemented locally:** [phase 2](../plan-2026-09-28-core-capability-owners.md)
  model runtime and external-call ownership, and
  [phase 3](../plan-2026-09-28-session-and-route-boundaries.md) tool turns,
  checkpoints and manager routes passed focused acceptance. This includes
  terminal model close, durable measurement and committed checkpoint adoption
  despite publication failure.
- **Audit correction:** the [single-response persistence plan](../plan-2026-09-28-single-response-persistence.md)
  removed an obsolete tested algorithm and restored common atomic budget
  observation/non-execution in the canonical response path. Store regressions
  and a real daemon crossing/park/resume scenario passed red-to-green. The
  terminal-empty regression passed. Two further test-only output transactions
  were removed in favor of canonical dispositions and batched results; their
  atomicity, generation, replay and fence scenarios passed after migration.
- **Latest correction:** the
  [settlement/lifetime plan](../plan-2026-09-28-settlement-and-worker-lifetime.md)
  closed nil-output result durability, attachment replay and heartbeat joining.
  The checkpoint constructor now lists dependencies explicitly. Independent
  code review found and closed one further committed-budget park handoff bug;
  its real-SQLite regression passed red-to-green.
- **Verification:** cold specification review and both independent code reviews
  are clean after correction. Focused persistence, session, daemon, manager and
  composition scenarios passed. Full `make test` and canonical `CI=true make ci`
  passed in an unrestricted environment with fixture Git configuration isolated
  from the invoking user's credentials. A stale prompt dependency in a migrated
  exact-cutoff test fixture surfaced in the first CI run; its focused regression
  and the subsequent full CI run passed. The audit records the evidence.
- **Next step:** preserve the new ownership boundaries during subsequent work;
  treat the deferred items in the audit as separate, evidence-driven changes.
- **Retained coordination:** the source audit supports keeping daemon startup,
  shutdown and typed producer routing, session activation safe points, and the
  shared SQL receiver. These coordinate distinct authorities rather than own
  unrelated mutable protocols; independent reviews confirmed this reading.
- **Completion evidence:** [the audit](refactoring-audit.md) tracks each goal
  requirement, flow, retained responsibility and final verification result.

## Keeping the direction intact

Before continuing after a handoff or context reset, read this document, the
current phase plan and the applicable architecture contracts; inspect the actual
branch and working tree. Resume from the checkpoint rather than treating the
most recent local task as the entire goal.

Every phase plan links here and states which desired result it advances.
Update the checkpoint at each completed phase or meaningful handoff: current
status, implementation reference, verification/review status, remaining work,
and next action. Keep only the current checkpoint; phase detail belongs in its
plan and rationale in ADRs. Do not use temporary files or conversation summaries
as the sole record of a decision or outstanding task.

Change the purpose or completion criteria only when the agreed direction
changes. This document records direction and continuity; it does not authorize
commits, publishing or other actions beyond the current user instructions.
