# ADR-0097: Orchestrator mode is a human-granted capability on an ordinary root

- **Status:** Accepted
- **Date:** 2026-11-17
- **Amended by:** [ADR-0100](0100-a-spawned-session-takes-a-tagged-model.md) — a spawn may name a tagged model
- **Amended by:** [ADR-0101](0101-an-orchestrator-ends-only-sessions-it-spawned.md) — an orchestrator can stop and kill the sessions it spawned
- **Amended by:** [ADR-0105](0105-built-in-managers-are-local-api-clients-in-process.md) — an operator client can also set or clear the mode with `SetOrchestratorMode`
- **Amended by:** [ADR-0106](0106-one-operator-every-surface-reaches-every-root.md) — an orchestrator reaches every manager root
- **Amended by:** [ADR-0107](0107-every-root-is-a-root.md)

## Context

The owner wants one of his sessions to coordinate the others: see them, write to them and start new ones in his
projects. An earlier "assistant / desk" feature did this with a special session kind plus an observer, relay, digest
and unread counters, and was removed for its complexity. Until now `source=agent` input only flowed from a parent to
its own subagent; letting one session write into any of the owner's roots, across projects, turns that path into a
trust boundary. [ADR-0095](0095-only-human-input-carries-session-commands.md) already makes every non-human input
plain text.

## Decision

Orchestrator mode is a durable `orchestrator: true` attribute on an ordinary manager-owned root. Only the human sets
or clears it, with `/orchestrate on|off` through the manager's controller; a model, a sent message, a manager's
create-time attribute map and subagents cannot. The mode survives restart and `/clear`. A root with the mode gets
three tools from its next run — `list_sessions`, `send_to_session`, `spawn_session` — and its subagents get none.
Each call re-checks the mode.

The authority ceiling is what the owner could do by typing plain text into a session in Telegram, minus commands. A
sent message is `source=agent` text: no slash command, activation grant, skill expansion, budget release or title
naming of an existing session. It can wake a session the human stopped, but not one parked at a budget checkpoint.
`spawn_session` only uses existing, non-hidden projects whose directory exists, never a coagent-created worktree; an
optional fresh worktree is created in the guarded no-execution mode. The new root is ordinary and independent and is
titled from its prompt at creation; its first input is a sent message rather than human input, and the human's chat
focus does not move.

Provenance is host-authored. The inbox row carries the sender's id and names. When the target accepts it, the
session prefixes a line saying which session wrote it and that it is not the human. It also writes the message,
marked with its sender, to the target's chat, so a human who switches there sees what was asked.

Targets get no tool and no reply path. The orchestrator gets no reply in this stage.

## Consequences

- One root, chosen by the human, can reach every live root of its manager. Its reach is the owner's own sessions,
  never another manager's.
- The capability is a mode, not a session kind: no agent type, role, hierarchy or shared task store. Turning it off
  removes it.
- The orchestrator is blind until a read tool and turn-outcome signals exist. It learns nothing from a send but the
  queue acknowledgement.
- A send to a stopped root activates it in the enqueue transaction, the way a due schedule does, so boot and
  runtime agree.
- Spawning refuses near daemon capacity (fewer than four free runner slots) so the human's own work stays
  admissible.

## Later stages

A `read` tool lets the orchestrator read another session's output log from its own per-target position, as untrusted
tool output. Signals come next: `send` arms a one-shot signal and `subscribe` a standing one. A signal fires only
on a turn outcome, at most one unread per watcher and target, is re-armed by `read`, and counts as a background
wake source for the watcher. Both extend the same tool package and daemon host. Neither adds tools to targets.

## Alternatives Considered

- **A dedicated orchestrator session kind or agent type.** That is the removed desk design: a second lifecycle, its
  own prompt and its own surfaces. A mode on an ordinary root reuses everything and can be switched off.
- **A reply tool on targets.** It gives every session a new capability and a new path into another session. Owner
  rejected it; later stages use a read and signals that leave targets untouched.
- **Letting the model enable the mode, or granting it per spawn.** That is an authority escalation reachable by
  prompt injection; only the human's manager path may grant it.
- **Delivering the spawn prompt as `source=user`.** That is today's create path, and it would hand the model human
  authority (`/config`, `/budget`) in the new session.
- **Showing the message in the target's chat at send time.** A message cancelled by `/stop` would appear without
  ever being read; acceptance time matches what the model saw, like scheduled turns.
