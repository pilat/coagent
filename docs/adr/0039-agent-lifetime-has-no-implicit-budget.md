# ADR-0039: Agent lifetime has no implicit budget

- **Status:** Accepted
- **Date:** 2026-09-02

## Context

Agent types assigned fixed iteration ceilings: explore stopped after 10 turns,
general after 25, and project-defined subagents after 50. Foreground `task` calls
also assigned every child a five-minute wall-clock deadline unless the model
selected another value. These limits were host policy presented as agent
behavior, not user-authorized budgets.

Iteration count is not a stable measure of useful work or cost. A broad read-only
investigation may need many cheap turns, while one model request or tool call can
consume most of a wall-clock allowance. A foreground deadline can also cancel a
healthy child while it is producing a result, resolve its durable obligation as
an error, and make the parent repeat work the child nearly completed.

Coagent already has controls at the boundaries that own the underlying risks:
provider and tool operation watchdogs, retry budgets, loop detection, explicit
stop and kill, admission limits, and the user-authorized root-tree budget from
ADR-0033. A high internal iteration ceiling remains useful only as a circuit
breaker for a defect that evades loop detection.

## Decision

Normal root and subagent work has no implicit iteration or wall-clock lifetime.
Agent type selects prompt, tool policy, mode, and model; it does not select an
iteration budget. The `task` tool exposes no timeout, and foreground children
wait for their durable completion obligation until they finish or an existing
explicit/operational terminal event occurs.

The agent loop retains a non-configurable 1000-iteration ceiling per activation
as an internal defect circuit breaker. It is not a supported budget or tuning
surface. Provider requests and individual tools retain their local watchdogs.
A root-tree budget armed before child work begins constrains that work. A budget
input sent while a foreground task is pending remains queued behind the durable
call, so stop or kill is the interruption mechanism for work already in flight.
Models cannot assign lifetime limits to their own children.

The obsolete `subagent_links.timeout_sec` column is removed rather than retained
as inert compatibility state. If `task.Execute` receives historical arguments
containing `timeout`, permissive decoding ignores the unknown field. Startup
continues to cancel unowned external task calls and recover owned calls from their
durable child links without re-executing them.

## Consequences

- Long but productive exploration and implementation can continue until the
  model returns a final answer.
- Foreground parents can remain suspended for a long-running child; this is the
  honest durable join contract rather than a hidden cancellation policy.
- A stalled provider request or tool call still fails at its operation boundary,
  and users retain stop, kill, and pre-armed explicit budget controls.
- Agent definitions and task calls have fewer knobs, while the subagent ledger
  loses one persisted field and requires a schema migration.
- The 1000-iteration circuit breaker can still terminalize pathological work;
  reaching it is an internal safety failure, not ordinary budget exhaustion.

## Alternatives Considered

- **Keep type-specific limits and tune the numbers.** Rejected because 10, 25,
  or 50 turns remain arbitrary proxies for useful work, cost, and elapsed time.
- **Make iteration limits optional in agent definitions.** Rejected because it
  preserves a product surface for a metric that ADR-0033 already rejects as a
  user budget and lets copied repository content silently truncate work.
- **Keep the five-minute default but allow task overrides.** Rejected because the
  default still kills healthy long-running children, while the model—not the
  user—chooses whether a task deserves more time.
- **Keep only an optional task timeout.** Rejected because stop, kill, explicit
  root budget, and operation-local watchdogs own the legitimate cancellation
  cases without adding a model-controlled child lifetime.
- **Remove every circuit breaker.** Rejected because a loop-detector defect must
  not permit unbounded provider calls. The high fixed ceiling remains a final
  host safety net outside normal behavior.
