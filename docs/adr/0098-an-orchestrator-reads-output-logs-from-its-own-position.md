# ADR-0098: An orchestrator reads output logs from its own position

- **Status:** Accepted
- **Date:** 2026-11-17

## Context

[ADR-0097](0097-orchestrator-mode-is-a-human-granted-capability-on-an-ordinary-root.md) lets an orchestrator write to
and start the owner's sessions but leaves it blind. It needs to see what a session produced, as the human does after
`/switch`, without reading the model transcript, without moving the human's chat cursor and without touching the
session it reads. The text it reads was written by another agent and its tools, so it carries no authority. Signals
(a later stage) will tell the orchestrator when to read; reading is the only way another session's text reaches it.

## Decision

`read_session` reads the target's output log, the rows Telegram delivers, not its transcript. Each row is labelled
answer, step, sent, scheduled, notice or closed; answer and step are told apart by the source-key phase the session
store already writes. Lifecycle rows without content are skipped.

The read position is a row per orchestrator and target in `session_watches`, holding the highest output-log id read.
The read selects the page and advances the position in one transaction. A page holds at most 30 rows and 16 000
characters. A read continues oldest first from the position, or, on request and on the first read of a target, shows
the newest rows and marks older ones read, saying how many it skipped. The result names the target's current state.

A target's id, old or new, resolves to its whole replacement lineage (predecessors and replacements); the lineage's
rows are read in id order and the position moves to the newest root. A killed target stays readable. An orchestrator's own replacement starts without positions.

The result is untrusted tool output, and `[#` inside row content is escaped so a target cannot forge a row label.
Reading writes nothing but the reader's own position. The tool declares `tool.DirectOnly`, so `batch` refuses it:
a page the position has moved past never shares one result budget with another call.

## Consequences

- The orchestrator sees answers, steps and notices exactly as the human's chat shows them, minus the human's own
  messages, which the output log does not hold.
- Nothing is read twice, and nothing is skipped without the model being told, with two exceptions: a crash between
  the read and the tool result's commit, and a context window under ~14k tokens, where the session's own result
  budget cuts a full page after the position has moved past it and the model sees only an omitted-characters marker.
- There is no backward paging; a skipped or clipped part stays visible to the human only.
- Signal state for the later stage is added to the same `session_watches` row.

## Alternatives Considered

- **Reusing the chat cursor.** It belongs to a manager chat and every selection write resets it; sharing it would move
  the human's chat.
- **Reading the target's transcript.** It holds tool results, host lines and compaction summaries the human never
  sees, and it changes on compaction.
- **A kind attribute on every output row.** Classifying from the existing source keys covers old rows and needs no
  change to any writer.
- **Positions in the orchestrator's attributes or transcript.** Attributes are rewritten whole and copied on `/clear`;
  the transcript is compacted. Neither is a per-target durable record.
- **Oldest-first only.** An old session's first read would spend a page on its beginning, and 200 unread steps would
  take several calls to reach the final answer.
