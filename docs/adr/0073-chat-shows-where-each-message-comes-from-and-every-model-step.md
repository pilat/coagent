# ADR-0073: The chat shows where each message comes from and every model step

- **Status:** Accepted
- **Date:** 2026-10-30
- **Amends:** [ADR-0072](0072-flat-chat-with-a-focus.md)

## Context

After living in the flat chat (ADR-0072), the owner reported two problems:

- **Messages had no origin.** A message carried no mark of which session wrote it. A
  row of the previous session still in flight during a switch read like a message of
  the session being entered.
- **Silent steps were invisible.** A model step that called tools without text
  produced nothing, and ADR-0072 has no progress cards. A long run looked dead until
  its next narration or final.

`/sessions` also needed a definition of "last activity" that covers both what a
session said and what it was told.

`/status` renders no current tool call at all, and the architecture says status and
events expose "never raw command text". A useful step line therefore can't be borrowed
from existing output. It needs a deliberate decision about what may leave the host.

## Decision

**Session header.** Every output-log row of a manager-owned root that the loop
delivers is sent as `#<id> <name>` on its own line, followed by the row's content.
This covers narration, step messages, finals, receipts, notices and the close row,
including the rows sent on entry. The name is the same `<project> · <title>` that
`/sessions` shows. It is added at delivery, never stored, and read once per batch.

**One message per model step.** Every accepted tool-calling response of a
manager-owned root writes one output-log message under the existing
`message:<id>:reply` key:
- it holds the model's text, if any, followed by a **tool trail**: one line per call,
  `→ <tool>` or ``→ <tool>: `<main argument>` ``;
- a known tool shows its main argument (bash command, file path, pattern, URL, query,
  task description, skill name), clipped to 80 characters after `logger.Redact`;
- `batch` expands to its inner calls;
- every other tool shows its name only.

Subagents publish nothing.

**Bash commands are shown.** The trail includes the bash command, redacted for secrets
the daemon has registered and clipped. This is a deliberate exception to "never raw
command text". It covers only step messages delivered to the owner's own chat and their
message events to the owning manager. `/status`, process status and previews, and every
other event kind keep the invariant.

**Last activity.** `/sessions` orders sessions by the latest of their newest
output-log row and their newest `user` or `agent` inbox input, compared by time.

## Consequences

- Every message says where it came from. A row of the old session that was already in
  flight during a switch is no longer mistaken for the new session's.
- Long runs are visible step by step. This partly reverses ADR-0072's "a long silent
  tool run shows nothing" without bringing back replaceable progress cards: each step
  is an ordinary persistent message.
- Chat volume grows to one message per tool-calling step. Transport backoff absorbs
  rate limits, but a busy session is noisy to read.
- A secret the model types literally into a command (one the daemon never registered)
  can reach the owner's messenger provider. The model's free text already could.
- Ordering moves on touch. Commands sent to a session (`/status`, `/compact`, …) lift
  it, because they are input. Renaming does not.

## Alternatives Considered

- **Store the header in the outbox content.** It would be simpler at delivery.
  Rejected: titles change after rows are written, and the log would carry presentation
  tied to one manager.
- **One message per tool call.** It is the most granular option. Rejected: a 25-call
  batch would flood the chat. The unit the user reasons about is the model's step.
- **Show only the tool name for bash, or only its first word.** Both keep the "never raw
  command text" invariant literal. Rejected by the owner: `→ bash` or `→ bash: go` says
  almost nothing, and the chat already carries the model's unfiltered text.
- **Order `/sessions` by the larger of the outbox and inbox ids.** Rejected: the two
  tables use independent sequences, so the comparison is meaningless. Time is the only
  common axis.
- **Collapse batch to `→ batch (N calls)`.** Rejected: batches are where the model
  bundles most of its reads and searches. Hiding them hides the work.
