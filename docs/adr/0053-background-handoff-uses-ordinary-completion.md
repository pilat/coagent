# ADR-0053: Background handoff uses ordinary completion

- **Status:** Accepted
- **Date:** 2026-09-11

## Context

[ADR-0050](0050-asynchronous-completions-use-session-inbox.md) made process and
background-subagent completion durable session input and added a cooperative
`<WAITING/>` assistant-text marker. The marker let a model suspend without an
unresolved tool call while preserving a wake source and an armed one-shot
budget.

Production use showed that the marker asks the model to participate in the
runtime protocol. A model must reproduce exact non-native syntax, emit no tool
calls beside it, and distinguish waiting from ordinary completion even though a
completed session is already eligible for asynchronous input. Repeating marker
instructions in prompts and tool results did not make that contract reliable
for weaker models.

The durable inbox already separates model control from completion delivery. An
ordinary final response releases the runner, and a later completion can start a
new activation. Removing marker suspension must still preserve the one-shot
budget across background work and retain completions when a fired budget parks
the tree.

## Decision

Independent process and background-subagent completions continue to enter the
exact owning session through `session_inbox`. Process terminal state and its
process-source row commit in one transaction. Background-subagent delivery and
its subagent-source parent row also commit in one transaction. The shared loop
promotes these bounded, origin-tagged user-role envelopes after a complete
assistant/tool-result batch and before the next model request. Completion
producers never append concurrently to the transcript and do not synthesize a
tool call.

After launching background work, the model continues independent work or ends
an ordinary response. A normal no-tool `finish=stop` completes the current
activation and releases its runner slot. There is no model-authored waiting
marker and no native background-wait tool. Completed roots remain eligible for
durable completion input; eligible child sessions retain their existing re-arm
rules. Stopped and errored sessions retain completion facts without waking, and
kill or clear cancels pending asynchronous input.

An armed root budget outlives an ordinary final response while the session tree
still owns an advertised running process, an undelivered non-blocking child
link, or pending process/subagent inbox input. Producer ledgers are checked
before the inbox so their atomic transition cannot disappear between reads.
Once no background obligation remains, the normal terminal release ends the
budget generation. If the budget fires first, its existing park protocol stops
the tree while preserving background processes; later completion remains
durable until the next ordinary root input releases the parked generation.

Background process guardians, output storage, explicit cancellation, startup
interruption, stop/kill suppression, inbox FIFO ordering, and completion
envelopes retain the ADR-0050 contracts.

## Consequences

- Models use only their native final-response and tool-call vocabulary; ending a
  response is sufficient to hand background work back to the daemon.
- A background-launch acknowledgement is an ordinary manager-visible final
  response. The later autonomous completion response remains replaceable
  progress because it does not answer a new manager-owned input.
- The session is `completed`, rather than `suspended`, between an ordinary final
  response and completion. Progress still derives background activity from the
  producer ledgers.
- One-shot budget lifetime depends on a new root-tree background-obligation
  projection rather than on marker-driven `RunResult.Suspended` alone.
- A fired budget remains stronger than completion wake: completion input is
  retained behind the stopped boundary until ordinary user input resumes it.
- Provider `length`, unknown finish, empty-response recovery, blocking calls,
  sleep, stop, kill, and cancellation keep their existing independent rules.

## Alternatives Considered

- **Keep and repeat `<WAITING/>`.** Rejected because exact assistant text is a
  second control protocol layered over native function calling, and only models
  that already follow it benefit from it.
- **Add `wait_for_background`.** Rejected because ordinary response completion
  already releases execution and completion already supplies the later turn. A
  completed-result suspension signal would add executor and crash-state
  machinery without adding a missing capability.
- **Infer suspension for every final response while background work exists.**
  Rejected because a user may ask an unrelated question while older background
  work remains. Ordinary completion answers that input without conflating it
  with a wait request.
- **Resolve independent completion as the original Bash/task tool result.**
  Rejected because explicitly background work is not an outstanding tool call;
  inventing one would violate transcript pairing and block useful intervening
  turns.
- **Release the one-shot budget on every ordinary final response.** Rejected
  because background work belongs to the same autonomous episode and can incur
  later model cost.
