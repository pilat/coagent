# ADR-0094: Budget park and length-retry cap keep no durable state of their own

- **Status:** Accepted
- **Date:** 2026-11-14
- **Amends:** [ADR-0033](0033-budget-is-one-shot-root-tree-checkpoint.md), [ADR-0052](0052-durable-model-attempt-outcomes.md), [ADR-0065](0065-daemon-owns-lifecycle-composition.md)

## Context

The reliability-diet inventory found that budget park phases, their owner,
the boot park pass and park retry timer coordinated short crash windows.
The owner accepted losing park resumption after a crash while retaining the
graceful finish of admitted work. The length-retry link also repeated identity
already established by transactional message ordering.
ADR-0065's retried parks and their durable generation and owner check go
with the phases.

Completion-check durability was examined and kept: its state carries normal
cross-run behavior, including a hidden candidate across suspension and the
confirmed answer delivered to a subagent's parent. Only its dead seed and an
unreachable invalidation branch are removed; ADR-0060 and ADR-0061 still apply.

## Decision

Keep budget accounting and the armed, fired and released states durable.
Firing still closes model admission in the step transaction. Keep parking in
the daemon process, coalesced per root through worker exit and joined at shutdown. A park waits
for in-flight tree runners without cancelling them, takes the tree fence and
rechecks liveness. Before each join round it abandons a superseded fire.

Under that fence, one conditional update moves the root to stopping only while
its budget is still fired and another lifecycle operation does not own it.
It includes completed roots whose background work fired the checkpoint and
matches any fired generation. A prior release, clear or re-arm skips cleanup.
Otherwise ordinary stop cleanup settles the tree while preserving background
processes. Both a skipped park and a completed park publish idle when no live
runner remains. Model-bound human input to a parking root receives the existing
parking notice; control and read-only commands still pass. After parking, the
next model-bound human input releases the fired budget and resumes the root.

A budget change committed while a park is in progress re-arms the budget and
the park is skipped; it used to be refused.

Every run ending on a fired budget requests a park. Boot reconciles only armed
budgets, synchronously parking newly observed overdue checkpoints before orphan
settlement. It does not resume unfinished parks. Deadline load or observation
failures still retry; park failures do not. Parking has no timeout and remains
cancellable through the daemon lifetime.

Derive length-recovery identity from stored row position: a recovery input is
the user row immediately following a rejected output-length attempt in the same
session. Both recovery and terminal-rejection queries retain their accepted
input boundaries. The rejected attempt and conditional recovery input share a
transaction; compaction marks rows without deleting them. No prompt-text match
or in-memory flag establishes recovery identity.

## Consequences

- A crash during a park loses that coordinator. The fired budget still closes
  admission; recovered runs may request another park and human input can release it.
- A crash after the root becomes stopping uses ordinary stop recovery, including
  cancellation of background processes. Failed park cleanup waits for that recovery.
- Re-arm during a park wins without losing accepted input or its mutation receipt.
- The one-length-retry cap survives suspension and restart without a link column.
- One migration drops the two budget park columns and the message retry link,
  preserving every surviving row and state constraint.

## Alternatives Considered

- **Cancel in-flight calls at the checkpoint.** Rejected because it loses tool
  results and creates ambiguous side effects, as recorded in ADR-0033.
- **Keep a durable parking phase.** Rejected because it retains the boot park pass
  and a coordinator for a crash window whose guarantee was dropped.
- **Use an in-memory length-retry flag.** Rejected because suspension and resume
  could reset the cap even without a crash.
- **Match recovery prompt text.** Rejected because text is not identity, as
  recorded in ADR-0052.
