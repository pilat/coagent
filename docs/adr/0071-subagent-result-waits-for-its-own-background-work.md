# ADR-0071: A subagent's result waits for its own background work

- **Status:** Accepted
- **Date:** 2026-10-12
- **Amends:** [ADR-0060](0060-wake-aware-model-completion-check.md), [ADR-0068](0068-one-completion-nudge-per-model-input.md)

## Context

ADR-0060 treats a no-tool `stop` with a durable wake source as an activation
yield, not as task completion. For a root this is right: the yield is published
with the 🟣 Background badge and the process completion wakes the root later.
For a subagent the same yield went through ordinary child finalization: the link
became `completed` and the parent received the yield text as the child's result.
When the child's background process finished, its completion re-armed the
already delivered child (ADR-0050 replaced ADR-0046's "a terminal subagent's
process wakes the root" with this re-arm), and the child's next stop became a
second result.

Production session 748/749 (2026-10-12) shows the cost. Foreground research
subagent 749 ended its turn with two of its own backgrounded `bash` searches
still running, one of them a five-minute `curl` nobody needed. Parent 748
received "Subagent #749 completed (32 iterations)…" as the `task` call result
and answered. Five minutes later the search completion woke 749 (activation 2,
link now non-blocking after re-arm), 749 wrote "the background searches
confirmed the report", the parent received it as `<subagent_completion>`, ran
another model generation and published a duplicate final answer, and its
progress card revived as "🟣 Background" after the answer. One assignment
produced two results and an extra parent model call.

A subagent's parent cannot act on a yield. Its only contract is the child's
result, so a yield must not look like one.

## Decision

For subagents only, a stop while the child still owns live background processes
(advertised, running `bash` processes of that exact session) is not a result.

- **Background-exit nudge.** On such a stop the host appends one host-authored
  user-role message listing each live process (ID, command excerpt, running
  time) and states both exits: cancel a process whose result is no longer needed
  with `cancel_process` and then give the final answer, or end the turn to be
  resumed when the process completes, after which the parent receives the result.
  For both exits it says the final answer replaces everything said before and
  must be the complete result with every finding and caveat, not only what the
  process changed: the incident's child answered with exactly such a delta. The
  stop is not terminal; the loop continues to the next model call.
- **One host nudge per generation.** The background-exit nudge and the ADR-0060
  completion nudge share ADR-0068's durable `completion_nudge_generation`
  marker. The background check runs first, inside the wake branch, so a single
  response never receives both. After a background-exit nudge, a stop that still
  has live processes yields, and a stop after `cancel_process` with nothing else
  to wait for is accepted as final without a completion nudge. A promoted process
  completion advances the generation and restores one nudge of either kind.
- **Yield, not result.** The subagent ledger owns the decision (ADR-0037):
  `subagent.Store.Finalize` does not terminalize a successful activation while the
  child owns a live advertised process. The link stays open with no outcome, the
  parent's foreground `task` call stays pending, and a background parent
  receives nothing. The child's session status is `completed` like any yield; a
  process completion enters the child's inbox and starts the same activation
  (activation sequence unchanged). The parent receives exactly one result, when
  the child stops with no live processes of its own; the ordinary completion check
  applies then. Errored activations still terminalize immediately.
- **Re-admission of a foreground child is fail-fast.** Foreground children are
  never queued for a runner slot: spawn already returns a capacity error rather
  than suspend the parent behind a queue. A foreground child that has already
  run and finds no capacity when its process completion (or the restart sweep)
  resumes it is terminalized by the ledger (`FailResume`) as an `error` outcome
  whose result states that the session capacity is full and carries the child's
  last answer. Delivery follows outside the tree lock, so the parent's `task`
  call returns now instead of waiting for a restart.
- **Stop and park include yielded children.** The stop plan also takes children
  whose session completed a yield but whose link is still open, so `/stop` and a
  budget park mark the link stopped like any active child. Park preserves the
  process; its later completion stays in the stopped child's inbox.

Roots are unchanged: background yield, 🟣 Background badge, wake on completion,
no background-exit nudge. Other wake sources of a subagent (an undelivered
non-blocking grandchild, a completion already queued in the inbox) keep their
current behavior.

ADR-0050's routing stands: a process completion always enters its owning
session's inbox, and a delivered `completed` child re-arms on it. After this ADR
that re-arm no longer follows an ordinary stop with live work, because such a
stop never delivers. Stop and kill cancel the tree's processes with suppressed
wake events, and daemon shutdown records them `interrupted` with a completion
owed to the still-open child, which startup recovery resumes.

## Consequences

- One subagent assignment produces one parent-visible result. The parent spends
  no model call on interim results or late duplicates.
- A foreground parent may wait for the full lifetime of the child's process
  (bounded by its deadline, at most thirty minutes). That is the intended price:
  the parent asked for a result that depends on that work.
- The nudge costs at most one extra child model call per input generation. The
  child must restate its answer in full after either exit; the nudge says so, and
  the earlier stop text is not carried over. Compliance remains a prompt property.
- The link can stay non-terminal while the session is `completed` and no runner
  exists. Readers that treat `spawned`/`running` links as live work (progress,
  restart sweep, budget obligation, kill) already see it; the stop plan had to be
  taught to.
- Under full child capacity a foreground child loses its post-wait turn and the
  parent gets an error result instead of the final answer. Accepted: it matches
  the spawn policy and keeps the parent from hanging.
- Restart between yield and process completion needs no new state: shutdown or
  crash recovery commits the interrupted process and its inbox row together, and
  the restart sweep starts the open child.
- The completion nudge marker now records "the host nudged a stop in this
  generation", not only the completion check. No migration.
- A subagent waiting only on its own non-blocking grandchild still delivers an
  interim result. That path keeps ADR-0060 behavior until evidence asks for it.

## Alternatives Considered

- **Kill background processes when a subagent gives its final answer.** Claude
  Code does this for foreground subagents. Rejected: it silently kills
  intentional long jobs such as test runs and builds that the child started
  precisely to wait on.
- **Deliver an interim result and re-deliver later.** Claude Code's
  background-subagent behavior and coagent's behavior before this ADR. Rejected:
  it produces duplicate results and an extra parent model call per late
  completion, as the incident shows.
- **Route late process completions to the root.** ADR-0046's original rule.
  Rejected: a raw process notice costs the root a model call, and the subagent
  that owns the context never finishes its own work.
- **Mark the yielded child `suspended`.** Rejected: `suspended` means a pending
  external call and drives waiting-slot publication and staged configuration
  apply; overloading it would change those paths. The ledger guard is atomic with
  the process ledger instead.
- **Queue a foreground child that cannot be re-admitted.** Rejected: spawn
  deliberately never queues foreground children, and a queued foreground child
  leaves its parent's call waiting on unrelated sessions to finish.
- **A separate durable marker for the background-exit nudge.** Rejected: two
  host nudges for one response, or one after another in the same generation,
  would reintroduce the nudge loop ADR-0068 closed.
