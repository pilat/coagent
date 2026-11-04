# ADR-0082: The manager is a groupless chat without an assistant

- **Status:** Accepted
- **Date:** 2026-11-04
- **Supersedes:** [ADR-0074](0074-assistant-is-an-ordinary-session-with-a-desk-head.md),
  [ADR-0075](0075-work-answers-reach-the-observer-through-its-inbox.md),
  [ADR-0077](0077-assistant-moves-the-user-in-never-out.md),
  [ADR-0078](0078-assistant-input-is-never-human-authority.md),
  [ADR-0080](0080-assistant-administers-work-at-open-and-hears-direct-exchanges.md),
  [ADR-0081](0081-assistant-forwards-and-the-host-relays-work-answers.md)
- **Amends:** [ADR-0076](0076-flat-chat-with-a-focus.md),
  [ADR-0079](0079-chat-shows-where-each-message-comes-from-and-every-model-step.md)

## Context

The assistant (`#0`, ADR-0072 through ADR-0081) was meant to be the one voice the
owner talks to: it routed words to work sessions, remembered projects and relayed
answers. Rounds of live use showed it costs more than it gives:

- it retold answers the owner had already read, and misrouted words between sessions;
- it needed five tables (`assistant_memory`, `assistant_focus`, `assistant_search`,
  `assistant_search_state`, plus the `sys_coagent` project), an observer, relay and
  forward layer inside the shared step commit, run tracking, a desk head and a
  silent-finish path in the session loop;
- every chat needed a read-tracking watermark per session (`chat_seen`) so the
  assistant's digest and the entry context could say what was unread.

The part the owner wants to keep is the flat chat itself (ADR-0076): one chat, a
focus, slash commands, every narration its own message, the `#N name` header and
the one-message-per-step tool trail (ADR-0079).

## Decision

**The assistant is removed, not hidden.** The `assistant` agent type, its package,
its prompt, its daemon host, `EnsureAssistantRoot`, the hidden `sys_coagent`
project, the observer/relay/forward layer, run tracking, the targeted schedule and
budget tools and the session head/silent-finish options are deleted. Every inbox
input from the user is human input.

**Focus NULL means no session.** `chat_focus.session_id IS NULL` is "nothing in
focus". Start-up binding inserts NULL and creates nothing. Plain text, media, voice
and every focus-scoped command with no focus get one manager-local line, `No session
in focus. /new, /spawn or /switch N.`, and are dropped. A killed focus falls back to
NULL. `/assistant` and `/switch 0` are gone; id 0 is never a session.

**One persisted cursor per chat, no read tracking.** `chat_seen` is dropped with
everything derived from it: unread counts, the digest, entry context and `· N new`.
`chat_focus.last_sent_id` holds the highest outbox id the chat has sent for its
current focus. Every focus write (switch, explicit re-switch, `/clear`, kill reset,
binding) sets it to NULL, and NULL means "entry owed". An entry sends the header
`→ #N name`, then the last 3 renderable rows of the session, oldest first, even if
they were sent before. It then sets the cursor to the session's newest outbox id.
Live rows after the cursor follow, and the cursor advances by compare-and-set after
each real send. Restart resumes from the persisted cursor and re-sends nothing that
was acknowledged. Outbox ids come from one table-wide AUTOINCREMENT, so one cursor
suffices. `/sessions` marks the focus and orders by last activity, capped at 30 with
`… and N more`.

**Management tools live on ordinary roots.** `config_edit` and `config_read` register
on every manager-owned root behind the existing `/config` grant (ADR-0031), as
`set_budget` does behind `/budget`. The secrets rule from the assistant prompt (never
ask for a credential; a pasted one must be rotated) moves into `config_edit`'s
description. The sandbox coaching is dropped. `schedule` stays the own-session tool
and `mcp_*` stays project-scoped.

**Migration 00059 deletes the assistant's graph.** It deletes every session of the
`sys_coagent` project and every `agent_type='assistant'` root with its descendants,
all their rows and the project row, the way Go migration 40 deleted `sys:coagent`.
It drops the assistant tables, strips the assistant-only attribute keys, seeds the
cursor from the focused session's watermark and drops `chat_seen`.

## Consequences

- One chat path remains: enter a session, read its last three messages, follow it
  live. There is no overview of other sessions beyond `/sessions`, and nothing
  announces work finishing elsewhere.
- Delivery stays at-least-once. A crash between a send and its cursor write repeats
  that one message. Rows a session wrote while out of focus, other than the last
  three, are never shown.
- Two quick `/switch N` to the same session can collapse into one entry, because both
  leave the same NULL cursor.
- The owner loses the assistant's memory notes, conversation and schedules on
  upgrade. A live recurring schedule on the assistant has to be recreated on a work
  session. The empty `~/.coagent/projects/sys_coagent` directory stays on disk and,
  with its hidden project row gone, shows in `/spawn` navigation.
- Global MCP servers (project id 0) can no longer be managed from the chat. Ordinary
  roots manage their project-scoped servers.
- Every manager-owned root carries the two config tool descriptions on every request.
  A model editing config no longer has the sandbox coaching, only the tool's own
  whole-document and sandbox summary.
- `projects.hidden` stays as a column (Go migration 40 owns it). Nothing writes it
  any more.

## Alternatives Considered

- **Hide the assistant behind a flag.** Keeps five tables, a hook in every step
  commit and a second chat semantics alive for a feature nobody uses. Rejected: the
  cost is in the shared paths, not in the visible surface.
- **Keep `chat_seen` for unread counts in `/sessions`.** The watermark per session
  is what drags in the digest, entry context and per-row bookkeeping. Rejected: the
  owner reads one session at a time and asks `/sessions` for activity, not counts.
- **Keep the cursor in memory.** The first pass after a restart would have to either
  re-send the tail or skip it blindly. Rejected: restart must re-send nothing, so the
  cursor is persisted on `chat_focus`.
- **One-row entry context, as ADR-0079 had.** It showed only where a session stopped
  when nothing was unread. Rejected: with no unread state, a fixed three-row window
  gives the same orientation on every entry.
- **Keep the `sys_coagent` project and the assistant's session rows as killed
  history.** The project row cannot be deleted while sessions reference it, because
  foreign keys are enforced during migrations. Rejected: "removed, not hidden" leaves
  nothing worth keeping, and migration 40 already set the precedent of deleting a
  retired project's whole graph.
- **Port the sandbox coaching into `config_edit`.** About 900 tokens on every request
  of every root. Rejected: the tool already carries the whole-document rule and the
  sandbox summary. Only the one-line secrets rule moves.
