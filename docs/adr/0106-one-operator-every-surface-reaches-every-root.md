# ADR-0106: One operator: every surface reaches every root

- **Status:** Accepted
- **Date:** 2026-11-24
- **Amended by:** [ADR-0107](0107-every-root-is-a-root.md)
- **Supersedes:** [ADR-0023](0023-manager-owned-session-publication.md)
- **Amends:** [ADR-0030](0030-durable-session-outbox.md)
- **Amends:** [ADR-0097](0097-orchestrator-mode-is-a-human-granted-capability-on-an-ordinary-root.md)
- **Amends:** [ADR-0101](0101-an-orchestrator-ends-only-sessions-it-spawned.md)
- **Amends:** [ADR-0104](0104-the-local-api-is-the-two-durable-ledgers.md)
- **Amends:** [ADR-0105](0105-built-in-managers-are-local-api-clients-in-process.md)

## Context

[ADR-0023](0023-manager-owned-session-publication.md) gave every manager-created root one durable owner, the
configured manager id in `manager_id`, so that one manager's conversations would not leak into another's. Several
later decisions built on that boundary: ADR-0030 ("Outbox/inbox rows also carry JSON attributes with an immutable
`manager_id`"), ADR-0097 (an orchestrator "can reach every live root of its manager … never another manager's"),
ADR-0101 ("re-check orchestrator mode and ownership on every call"), ADR-0104 ("each client is bound to its own owner
id … a client owns and sees only the roots it created") and ADR-0105 (reads of "every owned root").

coagent is single-tenant. The person who owns the machine, `config.yaml` and the sandbox is the only operator, and
every Telegram bot and API client is one of their surfaces. The boundary kept that one person's sessions apart from
themselves: a second bot could not see the first bot's roots, and an API client could not see Telegram's.

## Decision

- A root has no owner. Every manager and every operator API client lists, reads, tails and writes every root.
- `manager_id` stays on a root only as a marker. Its presence makes a manager root: one created through a manager or
  API client, or spawned by an orchestrator. A manager root has an output log, the config tools and orchestrator
  eligibility. Its value names the creating surface, and nothing compares it. Subagents and ownerless roots stay
  internal.
- Every manager-root event reaches every manager-root subscription, and each subscription keeps its own buffer.
- A chat keeps its own place. `chat_focus` stays keyed by the manager or API client id plus the chat id, which is why
  those ids stay distinct across managers and API clients. A root's own lifecycle (`/clear`, kill) still moves or
  ends every chat focused on it.
- An orchestrator root reaches every manager root. It still stops and kills only roots it spawned.
- `SessionDetails.owner_id`, unreleased, leaves `coagent.v1`. Its number and name are reserved.

## Consequences

- One operator sees one set of sessions from every surface. A root started in a Web UI continues in Telegram, and the
  reverse.
- Accepted: every bot's `/sessions` lists every live root under its existing 30-line cap.
- Accepted: `/switch`, `/kill N`, `/stop N`, `/title` and plain text reach any root; `/clear` or `/kill` on a root
  another chat focuses moves or ends that focus too.
- Accepted: an orchestrator's reach grows to every root.
- Accepted: every surface wakes on every root's events, which costs more re-reads; a subscriber more than 64 events
  behind drops a hint and catches up on its 5-second fallback read.
- Accepted: a subagent id is refused with `session N is not a root session` (`#N is not a live session` from focus,
  title and mode calls).
- Accepted: idempotency keys are scoped to the root and shared by every client; a client chooses keys unique across
  clients.
- Accepted: while the merge base still declares `owner_id`, `buf breaking` ignores field deletions in
  `coagent.proto`; that covers this change and a branch stacked on it until it rebases onto main.
- Accepted: output and inbox rows keep their copy of `manager_id` until the follow-up; nothing compares it.
- Accepted: live roots without the marker stay invisible to every surface, as before.
- Accepted: an orchestrator addressing a subagent id is told it is a subagent whether or not that subagent's root
  carries the marker.
- The marker is the last trace of ownership. A follow-up removes the ownerless-root kind, after which every root is
  simply a root.

## Alternatives Considered

- **Bind several surfaces to one owner id by configuration.** It keeps a tenancy mechanism, an `owner` key and a
  binding rule, for a product with one tenant.
- **Keep the owner checks as defence in depth.** They guard nothing in a single-tenant daemon, and every new surface
  would have to be bound to pass them.
- **Keep `owner_id` as an always-empty deprecated field.** v1 never shipped with it, and a dead field is permanent.
- **Remove the marker in the same change.** It changes the authority of legacy roots and regenerates every golden
  trace, so it is a decision of its own.
