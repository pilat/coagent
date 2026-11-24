# ADR-0101: An orchestrator ends only the sessions it spawned, through the human's command rows

- **Status:** Accepted
- **Date:** 2026-11-19
- **Amends:** [ADR-0095](0095-only-human-input-carries-session-commands.md), [ADR-0097](0097-orchestrator-mode-is-a-human-granted-capability-on-an-ordinary-root.md), [ADR-0099](0099-turn-outcomes-signal-watchers-through-armed-watch-rows.md)
- **Amended by:** [ADR-0106](0106-one-operator-every-surface-reaches-every-root.md) — the per-call check is orchestrator mode and a manager-root target
- **Amended by:** [ADR-0107](0107-every-root-is-a-root.md)

## Context

ADR-0097 gave an orchestrator no stop, kill or clear of other sessions, so the roots it spawns pile up until the
human kills each by hand. The owner reversed that for sessions the orchestrator spawned itself, and only for those.
The human's `/stop` and `/kill` are inbox rows: the row fences the session, keys the visible acknowledgement and
terminal rows, is recovered at boot and fires the turn-outcome signal. ADR-0095 says only human input
(`source=user`) carries session commands, so an agent-issued command has no row it could use.

## Decision

`spawn_session` writes a `spawned_by` attribute holding the caller's id in the transaction that creates the root.
Managers cannot set it at creation or later; `/clear` copies it to the replacement like every attribute but the
title. `stop_session` and `kill_session` accept a target only when its newest root's `spawned_by` is in the caller's
own replacement chain, and re-check orchestrator mode and ownership on every call.

The daemon host performs the call by writing the fixed text `/stop` or `/kill` as a `source=user` row in the
target's inbox, with the caller's sender attributes, and dispatching it through the same command handler as the
human's. `source=user` therefore means "carries the human's authority": the human delegates it with `/orchestrate`
for exactly these two commands on exactly the sessions that orchestrator spawned. Exactly one host function writes
such a row and its content is one of two constants; no model text reaches it. The acknowledgement row names the
sender, and the outcome signal to other watchers says which session stopped or killed the target; the actor itself
gets no signal and its armed one-shot is consumed.

The tool result reports what happened in the target's terms. A target with nothing running, no queued input and no
background work is not stopped. `stop_session` refuses a target whose budget is armed, because the stop would release the generation and
pre-empt a fire that waiting would have produced; `kill_session` is allowed, and a fired budget is never touched.
An armed generation is otherwise released whenever an episode completes, so a stop sheds nothing that waiting would
not.

## Consequences

- One stop flow and one kill flow: restart recovery, cleanup, rows and signals are the human's.
- `/kill` leaves directories, worktrees and project rows on disk, so an agent cannot destroy uncommitted work.
- Sessions spawned before this change carry no `spawned_by` and only the human can end them.
- The sessions a killed orchestrator spawned belong to nobody; the human's `/kill` is the only way to end them.
- Readers of `source=user` see a host-written command row exactly as the human's; anything that treats
  `source=user` as "the human typed this" must read the sender attributes.

## Alternatives Considered

- **A new `orchestrator` inbox source.** Honest on paper, but it needs a `session_inbox` rebuild and a second
  classifier in every gate that recognises lifecycle commands.
- **`source=agent` with a host-only command attribute.** Agent input activates a stopped root at enqueue, so it
  needs carve-outs there and in boot, wake, boundary and dispatch — a second command path.
- **An inputless stop call.** The only inputless stop is the ownerless one, which writes no output-log rows and has
  no boot recovery; copying the explicit stop without its row duplicates the flow.
- **A sent `↪ from` row naming the command.** It reads like a plain-text sent "/stop", which does nothing.
- **Keeping the armed budget on an agent stop.** Leaves an armed generation on a stopped root, a state the park
  path was not written for.
- **Watch rows as proof of spawning.** `send_to_session` and `subscribe` create the same rows for any session.
