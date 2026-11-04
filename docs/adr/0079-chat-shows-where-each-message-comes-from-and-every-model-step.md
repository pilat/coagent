# ADR-0079: The chat shows where each message comes from and every model step

- **Status:** Accepted
- **Date:** 2026-10-30
- **Amends:** [ADR-0076](0076-flat-chat-with-a-focus.md)
- **Amended by:** [ADR-0081](0081-assistant-forwards-and-the-host-relays-work-answers.md) — relayed work rows on `#0` get the `#N name` header at delivery
- **Amended by:** [ADR-0082](0082-manager-is-a-groupless-chat-without-an-assistant.md) — focus NULL means no session; one persisted cursor per chat replaces the watermark; entry sends the last 3 rows; no digest, no `#0`; management tools on ordinary roots

## Context

After living in the flat chat (ADR-0076), the owner reported four problems:

- **Entering a session was misleading.** On `/switch 777` the unread digest arrived
  first, and its line for #776 read like a message of another session. Only then did
  #777's tail arrive.
- **Messages had no origin.** A work message carried no mark of which session wrote it.
- **A read session showed nothing on entry.** If nothing was unread, entering it
  produced only the header.
- **Silent steps were invisible.** A model step that called tools without text produced
  nothing, and ADR-0076 had already removed progress cards. A long run looked dead until
  its next narration or final.

`/sessions` was also sorted by `sessions.updated_at` and unbounded.

The original mental model is tmux: you look at one pane, and only when you step back
to the overview do you see what happened elsewhere. The code showed one more fact.
`/status` renders no current tool call at all, and the architecture says status and
events expose "never raw command text". A useful step line therefore can't be borrowed
from existing output. It needs a deliberate decision about what may leave the host.

## Decision

**Digest only on the way out.** A focus change always sends the switch header
`→ #N <name>`. The unread digest follows only when the new focus is the assistant
(`#0`). Entering a work session sends the header, then that session's tail.

**Session header.** Every output-log row of a work root that the loop delivers is sent
as `#<id> <name>` on its own line, followed by the row's content. This covers
narration, step messages, finals, receipts, notices and the close row. The name is the
same `<project> · <title>` that `/sessions` shows. It is added at delivery, never
stored, and read once per batch. Assistant rows have no header.

**Entry context.** If the newly focused session has nothing unread, the loop sends its
newest already-seen renderable row after the header (and after the digest, on `#0`).
The watermark never moves for it. If unread exists, the tail is sent as before.

**One message per model step.** Every accepted tool-calling response of a
manager-owned work root writes one output-log message under the existing
`message:<id>:reply` key:
- it holds the model's text, if any, followed by a **tool trail**: one line per call,
  `→ <tool>` or ``→ <tool>: `<main argument>` ``;
- a known tool shows its main argument (bash command, file path, pattern, URL, query,
  task description, skill name), clipped to 80 characters after `logger.Redact`;
- `batch` expands to its inner calls;
- every other tool shows its name only.

The assistant keeps its text-only narration. Subagents publish nothing.

**Bash commands are shown.** The trail includes the bash command, redacted for secrets
the daemon has registered and clipped. This is a deliberate exception to "never raw
command text". It covers only step messages delivered to the owner's own chat and their
message events to the owning manager. `/status`, process status and previews, and every
other event kind keep the invariant.

**`/sessions` order and cap.** `#0` comes first. Then sessions with unread, then the
rest, each group ordered by last activity: the latest of its newest output-log row
and its newest user/assistant input, compared by time. The list shows at most 30
sessions, then `… and N more — ask the assistant to find one`.

## Consequences

- **What the user sees.**
  - Every work message says where it came from. A row of the old session that was
    already in flight during a switch is no longer mistaken for the new session's.
  - Entering a session never shows other sessions' news. The overview appears only
    on return to the assistant.
  - Entering a quiet session shows where it stopped, without changing unread state.
  - Long runs are visible step by step. This partly reverses ADR-0076's "a long silent
    tool run shows nothing" without bringing back replaceable progress cards: each step
    is an ordinary persistent message.
- **Volume.** Chat volume grows to one message per tool-calling step. Transport backoff
  absorbs rate limits, but a busy session is noisy to read.
- **Secrets.** A secret the model types literally into a command (one the daemon never
  registered) can reach the owner's messenger provider. The model's free text already
  could.
- **Context costs a send.** Entry context is one extra message per focus change into a
  read session.
- **Ordering moves on touch.** Commands sent to a session (`/status`, `/compact`, …)
  lift it, because they are input. Renaming does not.
- **Fresher work cards.** Step messages make the assistant's work
  cards report fresher "last output" times. Nothing reads their content.

## Alternatives Considered

- **Keep the digest on every switch.** It is ADR-0076's original rule, and the owner
  read its lines as messages of the session being entered. Rejected: the overview
  belongs to the moment the user steps back.
- **Store the header in the outbox content.** It would be simpler at delivery. Rejected:
  titles change after rows are written, and the log would carry presentation tied to
  one manager.
- **One message per tool call.** It is the most granular option. Rejected: a 25-call
  batch would flood the chat. The unit the user reasons about is the model's step.
- **Show only the tool name for bash, or only its first word.** Both keep the "never raw
  command text" invariant literal. Rejected by the owner: `→ bash` or `→ bash: go` says
  almost nothing, and the chat already carries the model's unfiltered text.
- **Show the trail for the assistant too.** Rejected: the assistant is the voice that
  talks to the owner. Its tool use (`send`, `open`, search) is mechanics, and its text
  already says what it is doing.
- **Order `/sessions` by the larger of the outbox and inbox ids.** Rejected: the two
  tables use independent sequences, so the comparison is meaningless. Time is the only
  common axis.
- **Collapse batch to `→ batch (N calls)`.** Rejected: batches are where the model
  bundles most of its reads and searches. Hiding them hides the work.
