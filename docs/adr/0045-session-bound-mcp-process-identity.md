# ADR-0045: MCP process identity is session-bound

- **Status:** Accepted
- **Date:** 2026-09-06

## Context

ADR-0041 made MCP catalogs outlive idle clients and keyed both resources by
resolved server configuration and work directory. That allowed ordinary
sessions in one project to share a subprocess and its discovered catalog.

Session shields add per-session process authority that can change over the
session's lifetime. Sharing a process across sessions would let one session
inherit another session's environment, lifecycle, or filesystem policy. Keying
only by the current shield value would still permit cross-session sharing and
would not identify which session must retire an old-policy process during a
raise.

## Decision

We keep the daemon-owned MCP pool, catalog caching, idle lifetimes, lazy
reconnection, and stable activation schemas from ADR-0041, but bind every live
client and catalog to one session's process-policy identity.

The pool key includes the resolved server configuration, work directory, and a
policy identity containing the owning session even when the runner is
unconfined. It also distinguishes filesystem-policy changes such as raising
shields. A client or catalog may be reused across activations of that same
session, but never by another session.

When shields rise, the daemon fences the session tree, releases its stack
references, retires and joins every client under that session's previous policy,
and removes the matching catalogs before confirming the transition. The next
activation discovers the server under the raised policy. Registry mutations
retain their existing server-scoped invalidation behavior.

## Consequences

- MCP subprocess authority, environment, failure state, and schemas cannot leak
  from one ordinary session into another.
- Sequential activations of one unchanged session retain the startup and prompt
  stability benefits of the pool and catalog cache.
- Two sessions using an identical server in the same project consume separate
  processes and catalog memory, trading efficiency for a clear authority
  boundary.
- Raising shields can take as long as releasing and closing the session's MCP
  clients, and fresh discovery can change the raised activation's tool inventory
  if the server no longer starts under the restrictive profile.
- ADR-0041 is superseded as a complete decision record; its catalog lifetime and
  schema-stability choices continue here with the narrower identity.

## Alternatives Considered

- **Keep cross-session work-directory pooling.** Rejected because process
  authority and lifecycle would no longer belong to the session whose tools use
  them.
- **Key by shield state but not session.** Rejected because equal booleans do not
  imply equal environment or lifecycle ownership, and targeted retirement would
  remain unsafe.
- **Remove pooling and restart MCP for every activation.** Rejected because
  repeated initialization delays the first model request and discards stable
  schemas without improving isolation beyond session-bound reuse.
- **Invalidate all clients for a server when one session raises shields.**
  Rejected because it disrupts unrelated sessions and is unnecessary once the
  policy key carries session identity.
