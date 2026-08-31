# ADR-0037: Subagent ledger owns cross-table transitions

- **Status:** Accepted
- **Date:** 2026-08-31

## Context

Subagent link state and SQL lived in `daemon`, while child creation,
terminalization, re-arming and completion delivery lived on the aggregate
`sessionstore.Store`. Two packages therefore mutated `subagent_links`, link
state crossed the boundary as strings, and the global orchestrator also acted
as a repository. Keeping every atomic operation in session-store would preserve
one SQL owner only by making an already broad persistence surface own another
bounded context.

## Decision

`internal/subagent` owns the typed link state, outcome, record and durable
ledger. It exposes `Store` for ordinary ledger access and `Transactions` for
the four cross-table transitions: child creation, activation terminalization,
completion delivery and re-arming after delivery. The composition root creates
both capabilities over the shared SQLite handle and injects them explicitly
into the daemon.

The transaction boundary may write `sessions`, `session_inbox` and `messages`
only when the subagent invariant requires those writes to commit with the link
mutation. A neutral `internal/transcript` record lets this boundary persist
completion rows without importing the session-store implementation. Daemon
coordinates runners and recovery but owns no subagent SQL.

## Consequences

Subagent lifecycle vocabulary has one owner, string conversions disappear from
the daemon/session-store seam, and the boundary lint can prevent SQL ownership
from drifting back into the orchestrator. Ordinary ledger reads and atomic
transitions remain separately mockable without runtime capability discovery.

The composition root and daemon constructor gain one explicit dependency. The
subagent transaction implementation knows the small SQL subset needed to join
its link mutation with session state; adding another cross-table operation must
justify itself as a subagent invariant rather than as repository convenience.

## Alternatives Considered

- **Keep link persistence in daemon.** Rejected because a runtime orchestrator
  would continue mixing concurrency ownership with raw SQL infrastructure.
- **Move every link operation into session-store.** Rejected because table
  centralization would hide the subagent state machine inside a mega-store and
  keep its contract coupled to unrelated inbox, outbox and budget capabilities.
- **Expose a generic transaction callback or dependency bag.** Rejected because
  it would hide the participating stores and permit arbitrary cross-context
  writes without a named invariant.
