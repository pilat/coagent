# ADR-0099: Turn outcomes signal watchers through armed watch rows

- **Status:** Accepted
- **Date:** 2026-11-17

## Context

An orchestrator ([ADR-0097](0097-orchestrator-mode-is-a-human-granted-capability-on-an-ordinary-root.md)) can write
to and start the owner's sessions and read their output logs from its own position
([ADR-0098](0098-an-orchestrator-reads-output-logs-from-its-own-position.md)), but it cannot tell when a session's
work is over, so it either polls or guesses. A session's turn can end in six ways — a final answer, an error, `/stop`,
a budget park, `/kill`, `/clear` — and these end in five different daemon places with no common point. A final
answer is not always the end: a turn that says "started it, will report" while a background process or subagent is
still running is not finished. The other session's text is untrusted and must reach the orchestrator only through
the read tool.

## Decision

One mechanism carries both replies and subscriptions: arms on the watch row, fired by turn outcomes, re-armed by
read. The `session_watches` row of an orchestrator (watcher) and a root (target) holds a one-shot arm, a standing arm
and a `signalled` flag. `send_to_session` and `spawn_session` set the one-shot arm in the transaction that queues the
input; `subscribe` sets the standing arm, `unsubscribe` clears it. A first arm positions a new watch at the target's
current output-log end; an existing watch keeps its position.

A turn outcome is one of the six above; for a final answer only when the target has no pending input and no
background wake source. The daemon calls one function at each of the five places an outcome happens. It fires every
row of the target that is armed and not signalled, whose watcher is live and still in orchestrator mode: one inbox
row per watcher, the one-shot arm consumed, `signalled` set. Nothing more is sent for that pair until the watcher's
next `read_session` of the target clears `signalled`. Each processed outcome also consumes one-shot arms covered by
an unread signal. A `/clear` of the target moves its rows to the replacement.

The model-facing signal is a host-authored line containing session ids, the project name, the outcome kind and a
`read_session` hint; it carries no target-authored text or title. It is a `source=subagent` row, which means a host-authored completion fact about
another session delivered to a session that waits on it. So it behaves as a background result does: it wakes an idle
orchestrator, waits in a stopped one, and never releases a budget. When the orchestrator accepts it, its own chat
shows a persist-only `🔔` row with the target's shared `ChatDisplayName`, including its title. For `/clear` it uses
the cleared session's display name and the replacement id. That chat row never enters the watcher's model transcript.
The target sees nothing.

An armed one-shot is a background wake source and a budget obligation of the watcher, so an orchestrator that says
"sent, waiting" is not nudged and keeps its budget; a wait on a session that waits on the caller does not count. A
standing subscription counts as neither. Two sessions cannot subscribe to each other.

## Consequences

- The orchestrator waits without polling and learns about each outcome once; reading is the only way to the text.
- A signal costs the watcher one short turn; several targets give several lines, never a combined one.
- Rings of three or more watching orchestrators, or orchestrators that keep sending to each other, are bounded only
  by budgets and the human.
- A crash between an outcome and its firing loses that signal; a one-shot stays armed until an outcome is processed,
  which consumes it even when an unread signal already covers the target.
- When the first arm creates a watch, rows older than that position are not readable through the tool.

## Alternatives Considered

- **A reply tool on targets.** Gives every session a capability and a path into another session; owner rejected.
- **Delivering the result text as a message.** Puts another agent's text into the orchestrator's inbox as input, the
  path that carries authority; the read tool returns it as untrusted tool output instead.
- **Notifications on state changes.** Running/idle flips include wakes that found nothing and command-only runs, and
  an idle target with background work is not finished.
- **A signal per output row.** Floods the orchestrator with steps; one unread signal until read keeps it to one line
  per pair however busy the target is.
- **A new `signal` inbox source.** Needs a `session_inbox` rebuild and six source lists edited for behaviour
  `source=subagent` already has.
- **Standing subscriptions as wake sources.** They live for days; counting them would switch off the completion check
  and keep budgets armed for every subscribed orchestrator.
