# ADR-0068: Session operations own complete tool turns and checkpoints

- **Status:** Accepted
- **Date:** 2026-09-28

## Context

Tool execution and compaction used the complete session receiver for registry,
transcript, model, activation, output and loop state. A scheduler already owned
parallel execution, and messageStore already owned projection persistence, but
neither owned the complete session operation connecting those primitives.
Moving only helper functions would preserve the same shared mutation authority.

Checkpoint commit also exposed a distinction its callers did not represent:
durable replacement could succeed before progress publication failed. That
failure skipped measurement invalidation and was reported as failed compaction,
even when the command had already completed in the same replacement transaction.

## Decision

Private session components own complete tool-turn and checkpoint operations,
using the existing scheduler, transcript primitive, model runtime and durable
transactions. Neither component receives the session receiver.

The tool-turn owner holds loop detection and orders execution, result commit
and progress. Activation is an explicit input; suspension and committed grant
consumption are explicit outcomes, including when a later progress effect fails.
The loop retains activation acquisition and terminal settlement.

The checkpoint owner holds command/deferral/attempt state and orders selection,
summary, replacement, measurement invalidation and outcomes. The loop chooses
the safe point and interrupts sleep before submitting a command. Committed
checkpoint state remains authoritative when progress publication fails: the
error is logged without reclassifying success, completing the command again or
losing its budget-fired outcome. Pre-commit failures still adopt nothing.

## Consequences

Changes to execution and checkpoint policy no longer require broad session
state access. Existing atomic store operations remain the durable authority.
Typed operation outcomes add explicit integration code, but avoid callbacks
that mutate the session from inside an extracted component.

Tests must cover errors before and after commit separately. Shared transcript
and model lock ordering remains necessary; decomposition does not make the
operations independent of their durable invariants.

## Alternatives Considered

- **Wrap only scheduling or summarization.** Leaves result/command commitment,
  grant handling and state adoption distributed across the old receiver.
- **Move activation state into the tool owner.** Requires callbacks for input
  acquisition and terminal expiry that can occur without any tool invocation.
- **Treat every returned error as no commit.** Contradicts durable replacement
  and allows presentation failure to change execution decisions.
