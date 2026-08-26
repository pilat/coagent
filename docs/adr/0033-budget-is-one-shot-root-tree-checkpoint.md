# ADR-0033: Budget is a one-shot root-tree checkpoint

- **Status:** Accepted
- **Date:** 2026-08-26

## Context

Unattended sessions can spend money and wall time across a root, its subagents
and compaction calls. An iteration limit is a poor user budget: roots and
children have different iteration costs, parallel children can bypass a
main-only count, and the model cannot reliably invent an acceptable spend or
duration on the user's behalf.

A permanent ceiling also has awkward recovery semantics. Once the ceiling is
crossed, even a user returning to inspect or redirect the task would remain
blocked. Conversely, increasing a limit must still work after autonomous work
has stopped. The checkpoint therefore needs durable tree-wide accounting and a
parking boundary, not a permanent policy attached to every future turn.

## Decision

There is no default budget. A root budget can be armed, replaced or cleared only
by a user-authorized `/budget` turn. It supports additional USD cost, additional
wall-clock duration, or both. The durable arm records UTC time and current
persisted lifetime tree cost as baselines; root, subagent and compaction work all
share that generation. Restart, compaction and model changes do not reset it.

The first reached condition fires the generation exactly once, closes admission
for new model, compaction and not-yet-started tool calls, lets already-executing
calls settle, and parks the current root tree through a durable idempotent
coordinator. Crossing responses do not start returned tools. Parallel calls
already in flight may overshoot, and the operator view reports that overshoot.

Firing disarms the limiter. It is a one-shot autonomous checkpoint: the next
ordinary model-bound user input releases the fired state and resumes only the
root without a budget. Read-only control commands do not resume it. Stopped
subagents retain history and require the existing explicit follow-up path.

Cost enforcement uses persisted priced usage and labels it accordingly. A model
or compaction path without catalog pricing is unavailable while a cost budget is
armed. Complete provider-call billing accounting is deferred as recorded in
`ai/techdebts.md`.

## Consequences

- A user, not the model, chooses whether and where autonomous work should stop.
- Cost and elapsed time survive restart and compaction and cover the complete
  session tree.
- The checkpoint fires only during the armed autonomous episode; ordinary user
  input after firing is not trapped behind an obsolete ceiling.
- Budget parking needs its own graceful drain owner. The existing `/stop`
  boundary cancels immediately and cannot be reused wholesale.
- In-flight parallel work may exceed the threshold; exact pre-charge
  reservation would require serializing or estimating provider work.
- A cost checkpoint can fire late when billed provider calls have no persisted
  usage row. The UI must not describe persisted cost as billing-grade spend.
- Automatic whole-tree thaw and iteration budgets remain outside this decision.

## Alternatives Considered

- **Let the model choose a budget.** Rejected because the correct spend and time
  tradeoff is user authority, not a model inference.
- **Use iteration counts.** Rejected because iteration cost differs by model,
  context and parallel topology and can be bypassed by child work.
- **Keep the ceiling armed after it fires.** Rejected because later user input
  could not naturally resume or redirect the task without a special escape
  protocol.
- **Resume the whole stopped tree on the next message.** Rejected because child
  deadlines, partial results and continuation policy are a separate lifecycle
  problem.
- **Cancel every in-flight operation at the threshold.** Rejected because it can
  lose already-produced results and create ambiguous external side effects.
- **Reuse `/stop` cleanup directly.** Rejected because its immediate runner
  cancellation prevents admitted work from settling before the park.
