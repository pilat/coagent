# ADR-0070: Every accepted response shares the budget observation transaction

- **Status:** Accepted
- **Date:** 2026-09-28

## Context

The accepted-response disposition protocol replaced an older budgeted-response
algorithm, but the older API remained exposed and tested. Its tests asserted
atomic budget crossing and non-execution results on a path production no
longer called. The canonical path observed budgets only in some disposition
branches, so an outputless tool response could persist its crossing cost and
execute the returned tools before a later admission check fired the budget.

That behavior contradicts [ADR-0033](0033-budget-is-one-shot-root-tree-checkpoint.md):
crossing responses must not start returned tools. The duplicated algorithm
concealed the gap rather than providing useful compatibility.

## Decision

Remove the unused budgeted-response API and algorithm. Every accepted-response
transaction inserts the paid attempt, observes its root budget exactly once,
and records non-execution results for returned tools when that budget fires or
has already fired. Disposition-specific policy consumes the common verdict.

Candidate validation and iteration updates remain in the same transaction;
failure in any later step rolls back usage, firing, checkpoint and skipped
results together. Budget suppression prevents ordinary nudge/final/error
effects from replacing parking. The returned record includes the budget owner
needed to schedule that park. The existing host-only checkpoint remains the
visible output; paid model text remains durable evidence.

After commit, the session adopts the fired verdict and schedules the park
before reloading its transcript. A failed post-commit read cannot turn a fired
response into an ordinary session error or leave the committed park request
without its worker. The next activation reloads the transcript.

## Consequences

Outputless tools and empty responses now enforce the same recorded budget
contract as candidate and final responses. Tests call the production protocol,
and their reference model derives crossing from input costs independently.
Rejected attempts and compaction retain their distinct transaction contracts
and shared threshold computation; no new schema or budget policy is introduced.

## Alternatives Considered

- **Keep the obsolete API for tests.** Preserves a misleading proof over a
  protocol no real session executes.
- **Give migrated fixtures synthetic output to trigger observation.** Hides
  the production gap for ordinary outputless tool calls.
- **Wait for a timer or the next model admission.** Allows returned tools to
  execute after their response already crossed the persisted-cost threshold.
