# ADR-0095: Only human input carries session commands

- **Status:** Accepted
- **Date:** 2026-11-17

## Context

The next stage lets a root session write `source=agent` text into any of its
owner's roots. A producer's text must not acquire human authority merely by
beginning with a slash. Until now, `/help`, `/status`, `/schedules`, `/compact`,
`/skill` (including skills hidden from the model), and the slash turn barrier
worked for any source. `/stop`, `/clear`, `/kill`, and tool activation were
gated individually, leaving an inconsistent trust boundary.

## Decision

The session boundary reads commands only from human input (`source=user`) and
scheduled turns (`source=schedule`). Agent, process, and subagent input reaches
the model verbatim as ordinary text: it invokes no session command, expands no
skill, and creates no slash turn barrier. Accept this text rather than rejecting
it, including unknown skill names.

Human input keeps its existing behavior. Scheduled turns also keep their
existing handling: exact `/help`, `/status`, and `/schedules` receive host
answers without a model turn; `/compact [focus]` requests compaction. Other slash
text, including `/skill` and `/stop`, reaches the model verbatim through the
schedule path without skill expansion. Every slash-prefixed scheduled input
remains a FIFO turn barrier.

## Consequences

Non-human text cannot command a session, expand a skill, or split a turn through
slash syntax. It waits behind outstanding non-sleep calls like ordinary text.
A parent can no longer compact its child by sending `/compact`; automatic child
compaction remains available. Schedules retain their command handling without
becoming human input.

## Alternatives Considered

- Reject non-human slash input: loses useful text and reveals which commands
  exist to the producer.
- Gate each command separately: this scattered approach created the gap and
  makes future commands easy to leave unguarded.
- Escape or prefix the text: changes what the model reads without improving the
  source-based authority boundary.
