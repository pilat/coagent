# ADR-0076: The manager is one flat chat with a focus over an append-only output log

- **Status:** Accepted
- **Date:** 2026-10-29
- **Supersedes:** [ADR-0028](0028-telegram-manager-owns-one-forum-target.md),
  [ADR-0035](0035-model-input-generations-delimit-replaceable-output.md),
  [ADR-0060](0060-telegram-service-topics-own-daemon-management.md)
- **Amends:** [ADR-0029](0029-set-manager-is-a-presence-aware-upsert.md),
  [ADR-0030](0030-durable-session-outbox.md),
  [ADR-0036](0036-explicit-stop-has-a-durable-terminal-output.md),
  [ADR-0061](0061-completion-check-publishes-the-candidate-answer.md),
  [ADR-0074](0074-assistant-is-an-ordinary-session-with-a-desk-head.md),
  [ADR-0075](0075-work-answers-reach-the-observer-through-its-inbox.md)

## Context

The Telegram manager was built on forum topics: a service topic hosting a management
session, an Assistant topic, one topic per session, inline keyboards, and in-place
edits. The core supported those edits with "replaceable" outputs (progress cards
redrawn on a timer, finals with a budget/model footer), a model-input-generation stamp
deciding when an edit may reuse earlier messages, and a durable per-row delivery queue
with receipts so the manager knew which messages to edit.

The product needs a manager that works on any text messenger: no groups, threads,
buttons or edits. Once the assistant became an ordinary session (ADR-0074) that
learns work answers through its inbox (ADR-0075), the topics stopped earning their
cost. Every user-visible mechanism above existed only to feed them.

## Decision

**Transport.** Each manager owns exactly one chat. It sends only new text messages
and reads slash commands. It never edits, never deletes, never shows a keyboard.

**Focus.** `chat_focus(manager_id, chat_id) → session_id` holds one live
manager-owned root, or NULL, meaning the assistant (`#0`).
- `/new`, `/spawn` and `/gwt` create a session and focus it. `/switch N` (N is a
  session id) and `/assistant` move the focus.
- `/kill N` and `/stop N` reach a session without moving the focus. Every other
  command, and plain text, goes to the focused session.
- `/clear` moves the focus to the successor in the same transaction.
- A killed focus falls back to the assistant.

**Delivery.** `session_outbox` becomes an append-only log of what the user sees.
`chat_seen(manager_id, chat_id, session_id) → last_outbox_id` is the only delivery
state.
- One transport-neutral loop per chat sends the focused session's rows after the
  watermark, in order, and advances the watermark after each real send.
- Entering a session and live delivery are the same path.
- A focus change first sends a header and an unread digest (one line per other live
  session with rows after its watermark), then the new focus's tail.
- Non-focused sessions are never touched.

Delivery is at-least-once, and errors fall into three classes:
- **Transient:** retry in memory, watermark kept.
- **Row-permanent:** plain-text fallback inside the transport. Then a warning line
  sent as a probe. A delivered probe skips the row; a failed probe means the chat
  itself is broken.
- **Chat-level:** pause with backoff, watermark kept.

**Core.** All outputs are persistent and every narration is its own message. These go
away:
- replaceable output and the generation stamp on rows;
- progress cards and their timer;
- output readiness;
- the final footer;
- the claim/ack/retry/block queue with its receipts and manager bindings.

`/status`, `/budget` and `/model` report on request.

**Management.** The management session, its service topic and the
`management_surface` attribute are removed. The assistant takes the management
instruction and the owner tools:
- `schedule` on itself;
- global-only `mcp_*`;
- `config_edit` with a new `config_read` riding the same `/config` grant;
- `set_budget` on itself.

The hidden `sys_coagent` project stays as the assistant's home.

## Consequences

- One code path serves "show me what I missed" and "show me what happens now". Unread
  state is a query, not a registry, and no delivery state is kept per row.
- Any text messenger can host the manager. The transport contract shrinks to "send
  text, classify the error".
- Only one session is visible at a time. The user switches deliberately, and other
  sessions announce themselves only through the digest on the next switch. Live
  progress of background work is no longer pushed.
- Delivery is at-least-once: a crash between send and watermark repeats a message.
- Progress cards are gone. A long silent tool run shows nothing until its next
  narration or final, and the completion-check candidate stays invisible until it is
  confirmed.
- Narration is now published on every tool-calling response, the assistant's
  included, so prompts must treat pre-tool text as a real message.
- `coagent status` loses delivery-backlog health. A blocked chat shows up only in
  logs.
- Rows of killed sessions that were never read are never shown. Re-pointing a manager
  to another chat replays history, because the new chat has no watermarks.
- The upgrade needs data migrations:
  - seed watermarks from delivered rows;
  - delete progress rows;
  - retire management roots and move their schedules to the assistant;
  - rebuild the outbox without its state machine;
  - recompute fingerprints for the renamed message type.
- The assistant now acts on the host directly (MCP, schedules, config, budget). This
  narrows ADR-0074's "the assistant delegates the world" to "the assistant delegates
  project work".

## Alternatives Considered

- **Keep topics and add a flat mode beside them.** Two delivery models, two command
  sets and the edit machinery would all stay alive for one transport. Rejected: the
  forum model is exactly what blocks other messengers.
- **Keep the claim queue and filter it by focus.** Rows of non-focused sessions would
  either block the FIFO head forever or have to be acked without being sent. Acking
  without sending destroys the very unread state the digest needs. Rejected in favour
  of a watermark over the log.
- **Derive unread from the transcript table `messages`.** It holds model-facing rows
  (timestamped user turns, nudges, compaction summaries, rejected attempts), but not
  the tool receipts, command results and notices the user actually sees. Rejected:
  `session_outbox` is the user-visible log.
- **Skip a permanently failing row unconditionally.** One rule, never stalls. Rejected:
  a 400 that is really about the chat (lost rights, migrated group) would silently
  advance the watermark through the whole tail. The warning probe tells a bad row from
  a dead chat.
- **Keep a management session reachable by its own command.** It would preserve the
  old split, but it adds a second permanent pseudo-session beside the assistant, and
  the flat chat already has one place for the user to talk to coagent. Rejected.
- **Give the assistant the general `read` tool for config edits.** Simpler, but it
  opens arbitrary host file access to the voice that is meant to delegate. Rejected in
  favour of a narrow `config_read` gated by `/config`.
