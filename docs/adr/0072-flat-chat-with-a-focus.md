# ADR-0072: The manager is one flat chat with a focus over an append-only output log

- **Status:** Accepted
- **Date:** 2026-10-29
- **Supersedes:** [ADR-0028](0028-telegram-manager-owns-one-forum-target.md),
  [ADR-0035](0035-model-input-generations-delimit-replaceable-output.md),
  [ADR-0060](0060-telegram-service-topics-own-daemon-management.md)
- **Amends:** [ADR-0029](0029-set-manager-is-a-presence-aware-upsert.md),
  [ADR-0030](0030-durable-session-outbox.md),
  [ADR-0036](0036-explicit-stop-has-a-durable-terminal-output.md),
  [ADR-0061](0061-completion-check-publishes-the-candidate-answer.md)
- **Amended by:** [ADR-0073](0073-chat-shows-where-each-message-comes-from-and-every-model-step.md) — session header on every delivered row, one message per tool-calling step, how `/sessions` measures activity

## Context

The Telegram manager was built on forum topics: a service topic hosting a management
session, one topic per session, inline keyboards, and in-place edits. The core
supported those edits with "replaceable" outputs (progress cards redrawn on a timer,
finals with a budget/model footer), a model-input-generation stamp deciding when an
edit may reuse earlier messages, and a durable per-row delivery queue with receipts so
the manager knew which messages to edit.

The product needs a manager that works on any text messenger: no groups, threads,
buttons or edits. Every user-visible mechanism above existed only to feed the topics.
The owner works in one session at a time, so the chat needs to know which session the
user is in and how far that session has been shown, and nothing more.

## Decision

**Transport.** Each manager owns exactly one chat. It sends only new text messages
and reads slash commands. It never edits, never deletes, never shows a keyboard.

**Focus.** `chat_focus(manager_id, chat_id) → session_id` holds one live
manager-owned root, or NULL: no session in focus. Start-up binding inserts NULL and
creates nothing.
- `/new`, `/spawn` and `/gwt` create a session and focus it. `/switch N` (N is a
  session id) moves the focus. Id 0 is never a session.
- `/kill N` and `/stop N` reach a session without moving the focus. Every other
  command, and plain text, goes to the focused session.
- `/clear` moves the focus to the successor in the same transaction.
- A killed focus falls back to NULL.
- With no focus, plain text, media, voice and every focus-scoped command get one
  manager-local line, `No session in focus. /new, /spawn or /switch N.`, and are
  dropped.

**Delivery.** `session_outbox` is an append-only log of what the user sees.
`chat_focus.last_sent_id` is the only delivery state: the highest outbox id the chat
has sent for its current focus.
- Every focus write (switch, an explicit switch to the current focus, `/clear`, the
  kill reset, binding) sets the cursor to NULL. NULL means an entry is owed.
- An entry sends the header `→ #N name`, then the session's last 3 renderable rows,
  oldest first, even if they were sent before. It then sets the cursor to the
  session's newest outbox id.
- Live rows after the cursor follow in order. The cursor advances by compare-and-set
  after each real send.
- A restart resumes from the persisted cursor and re-sends nothing that was
  acknowledged. Outbox ids come from one table-wide AUTOINCREMENT, so one cursor per
  chat suffices.
- Sessions out of focus are never touched: they stay silent until the user enters
  them.

Delivery is at-least-once, and errors fall into three classes:
- **Transient:** retry in memory, cursor kept.
- **Row-permanent:** plain-text fallback inside the transport. Then a warning line
  sent as a probe. A delivered probe skips the row; a failed probe means the chat
  itself is broken.
- **Chat-level:** pause with backoff, cursor kept.

**Core.** All outputs are persistent and every narration is its own message. The
log carries four row types: `message`, `session_opened`, `session_replaced` and
`session_closed`. There is no replaceable output, no generation stamp on rows, no
progress card or timer, no output readiness, no final footer, and no
claim/ack/retry/block queue with receipts or manager bindings. `/status`, `/budget`
and `/model` report on request. `/sessions` lists the manager's live sessions, marks
the focus, orders them by last activity and shows at most 30, then `… and N more`.

**Management.** There is no management session, no service topic, no
`management_surface` attribute and no hidden management project. Management tools
live on every manager-owned root:
- `config_edit` and a new `config_read`, both behind the `/config` grant
  ([ADR-0031](0031-user-commands-grant-one-turn-tool-authority.md));
- `set_budget` behind `/budget`;
- `schedule` on the session itself;
- project-scoped `mcp_*`.

The secrets rule (never ask for a credential; a pasted one must be rotated) lives in
`config_edit`'s description.

**Upgrade.** The migration deletes progress rows and the hidden `sys_coagent`
project's whole graph (its sessions, their rows, its memories and MCP servers, the
project row), the way Go migration 40 deleted `sys:coagent`, together with any
`management_surface` root elsewhere and its descendants. It strips the
`management_surface` and `telegram_topic_id` attributes and rebuilds the outbox as the
four-type log, keeping its AUTOINCREMENT high-water mark. It drops `manager_bindings`
and `sessions.manager_reply_pending`.

## Consequences

- One chat path: enter a session, read its last three messages, follow it live. There
  is no overview of other sessions beyond `/sessions`, and nothing announces work
  finishing elsewhere.
- Any text messenger can host the manager. The transport contract shrinks to "send
  text, classify the error".
- Delivery is at-least-once: a crash between a send and its cursor write repeats that
  one message. Rows a session wrote while out of focus, other than the last three, are
  never shown.
- Two quick `/switch N` to the same session can collapse into one entry, because both
  leave the same NULL cursor.
- Pointing a manager at another chat starts that chat with no focus.
- Progress cards are gone. A long silent tool run shows nothing until its next
  narration or final, and the completion-check candidate stays invisible until it is
  confirmed.
- Narration is published on every tool-calling response, so prompts must treat
  pre-tool text as a real message.
- `coagent status` has no delivery-backlog health. A blocked chat shows up only in
  logs.
- Every manager-owned root carries the config tool descriptions on every request. A
  model editing config has only `config_edit`'s own description: the whole-document
  rule, the sandbox summary and the secrets rule.
- Global MCP servers (project id 0) cannot be managed from the chat. Roots manage
  their own project's servers.
- The upgrade loses the management sessions' history, schedules and memories. The
  empty `~/.coagent/projects/sys_coagent` directory stays on disk and, with its hidden
  project row gone, shows in `/spawn` navigation. `projects.hidden` stays as a column
  (Go migration 40 owns it); nothing writes it.

## Alternatives Considered

- **Keep topics and add a flat mode beside them.** Two delivery models, two command
  sets and the edit machinery would all stay alive for one transport. Rejected: the
  forum model is exactly what blocks other messengers.
- **Keep the claim queue and filter it by focus.** Rows of sessions out of focus
  would either block the FIFO head forever or have to be acked without being sent.
  Rejected in favour of one cursor over the log.
- **Keep a read position for every session the chat has shown.** It would let the
  chat report which other sessions have new output. Rejected: the owner reads one
  session at a time, and that report costs bookkeeping on every row for a question
  `/sessions` answers by activity.
- **Keep the cursor in memory.** The first pass after a restart would have to either
  re-send the tail or skip it blindly. Rejected: restart must re-send nothing, so the
  cursor is persisted on `chat_focus`.
- **Show only the newest row on entry.** Rejected: a fixed three-row window gives the
  same orientation on every entry, whatever the session did while out of focus.
- **Deliver from the transcript table `messages`.** It holds model-facing rows
  (timestamped user turns, nudges, compaction summaries, rejected attempts), but not
  the tool receipts, command results and notices the user actually sees. Rejected:
  `session_outbox` is the user-visible log.
- **Skip a permanently failing row unconditionally.** One rule, never stalls.
  Rejected: a 400 that is really about the chat (lost rights, migrated group) would
  silently advance the cursor through the whole tail. The warning probe tells a bad
  row from a dead chat.
- **Keep a management session reachable by its own command.** It would preserve the
  old split, but it keeps a permanent second kind of session with its own
  instructions and history. Rejected: the management tools on every root, behind the
  grants that already exist, give the same capability in the session the user is in.
- **Keep the `sys_coagent` project and its sessions as killed history.** The project
  row cannot be deleted while sessions reference it, because foreign keys are
  enforced during migrations. Rejected: nothing in it is worth keeping, and migration
  40 already set the precedent of deleting a retired project's whole graph.
- **Carry the management session's sandbox coaching into `config_edit`.** About 900
  tokens on every request of every root. Rejected: the tool already carries the
  whole-document rule and the sandbox summary. Only the one-line secrets rule moves.
