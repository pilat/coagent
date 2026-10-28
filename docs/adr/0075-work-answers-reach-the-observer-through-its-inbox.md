# ADR-0075: Work answers reach the observer through its inbox in the work's own commit

- **Status:** Accepted
- **Date:** 2026-10-28
- **Amends:** [ADR-0074](0074-assistant-is-an-ordinary-session-with-a-desk-head.md)

## Context

The assistant is the single voice in the Assistant topic: the user does not read work
topics, so every answer to an instruction the assistant sent must come back to it. ADR-0074
kept the observation subsystem from the separate-loop design: an `assistant_sends` ledger
row per instruction, a window per send bounded by the next accepted user input of the work
root, and a decision function that re-read that window after the fact. Because the
decision ran after the work's commit, it needed callers on every path where a work root
can stop producing output — runner exit, kill, explicit stop, stop completion for queued
and yielded roots — and a boot sweep for the crash window between the work's commit and
the observation. That was about 415 production lines and the main coupling between the
assistant and the daemon.

The core's subagent delivery (`subagent_links`, ADR-0037) does not fit: it is a tree
ledger between a parent and its children, and assistant work roots are deliberately
parentless roots with their own topics that the user may also address directly.

What the observation actually needs is narrower: the work root's commit already knows,
in its own transaction, that it is writing a final or a terminal error. Whether that final
was a background yield is known only in memory at that moment; nothing durable records it.

## Decision

An inbox row may carry `observer_session_id`, the id of the root that wants its answer.
The assistant writes it on every instruction it enqueues. `sessionstore` turns it into
input for the observer inside the work root's own transactions; no other package takes
part.

- **Answered.** A non-yield final output of a root W enqueues, in the same commit, one
  `source=agent` input into each observer A whose ref rows of W fall in the open window.
  `session` marks yield on the output in memory; yield finals notify nobody. The event
  carries every final W wrote from the earliest open ref row through the committing final,
  joined oldest first as before: a closing final often only points back at an earlier one,
  typically the yield text written before a background process.
- **Failed.** A commit that writes status `error`, and a fresh session-start failure
  receipt, enqueue a failed input to the observers of W's pending ref rows and of its
  accepted ref rows after the anchor. A failure does not close the window.
- **Anchor.** The window's lower bound is the newest *answered* input A already received
  for W: its `observed_message_id`. Bounds compare `messages.id` of W only — the anchor
  against a ref row's `accepted_message_id`, and the final's own message id as the upper
  bound. Only answered events move the anchor, and a cancelled one does not: `cancelled`
  is the only input state meaning the event never reached A's runtime (A was stopped or
  killed while it was pending). `handled` and `rejected` are outcomes A's runtime chose,
  and sending the same content again would meet them again.
- **Silent lifecycle.** Kill, clear and stop notify nobody: either the user did it in the
  work topic or the observer itself asked for it.
- **Durability** is the shared transaction. The observer is woken after commit like any
  enqueued session. An observer that does not accept input is skipped without a log line:
  it is killed or stopping, and the card's "awaiting answer" stays as the signal. Any other
  enqueue failure rolls back the work's commit.
- **Capability.** `observer_session_id` lets one session's commit write into another
  root's inbox, so only in-process code writes it, through `EnqueueTx`; no controller path
  accepts inbox-row attributes, and the notifier never reads the ref from session
  attributes.

## Consequences

- The `assistant_sends` table, `observe.go`, the daemon's observation calls and the boot
  sweep are gone; the daemon no longer learns anything about observation.
- The hottest core path changes: `Store.Commit` and `RecordSessionStartFailure` register
  their transactions for enqueue and wake enqueued sessions after commit. A commit with no
  ref rows writes and wakes exactly what it did before.
- Every final of W that answers at least one open ref row becomes an event, including a
  final that also answers the user's own message typed into the work topic in the same
  activation. A final that answers only the user's direct messages produces nothing.
- The desk's "awaiting answer" and "assistant-touched" are derived from ref rows and the
  anchor instead of the ledger; awaiting is hidden for stopped, killed and errored roots,
  as the ledger settled those sends.
- Correctness of the anchor depends on `session_inbox` rows never being deleted. Today they
  are deleted only by the one-time management-project migration (version 40). A future
  inbox retention policy must keep, per observer and work root, the newest answered input,
  or the anchor falls back to an older one and old ref rows are reported again.
- A skipped or cancelled event leaves the anchor in place, so the next final also reports
  those rows; until then the card shows the work as awaiting an answer.
- A second start failure on a later instruction is not reported: the start-failure
  receipt is keyed by the earliest pending input. The card shows the work as failed.
- A failure no longer carries the finals written before it; a stopped or killed work root
  sends no "failed" line.

## Alternatives Considered

- **Reuse the subagent ledger** by making work roots children of the assistant or by
  allowing links between roots. Rejected: children share the assistant's tree lock and
  kill cascade and lose their own topic and direct user access; cross-root links move the
  same complexity into the most fragile ledger of the core.
- **Put the observer reference on the work root.** Rejected: every final of the root,
  including answers to the user's direct conversation in the work topic, would become an
  event, and ADR-0074 forbids silent turns, so each would produce a line in the Assistant
  topic; a `send` into a root the assistant did not open would never be reported.
- **Persist a yield marker on the final output** and bound the window by the previous
  non-yield final. Rejected: a second durable fact and parsing the message id out of the
  output key, where the answered event already records the bound.
- **Mark ref rows as answered.** Rejected: the send ledger moved into inbox attributes,
  with the mutable state this decision removes.
- **Keep the ledger and drop only the sweep.** Rejected: the decision after the fact still
  needs a caller on every terminal path, and the crash window between the work's commit
  and the observation remains.
