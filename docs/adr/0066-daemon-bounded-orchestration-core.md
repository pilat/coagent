# ADR-0066: Daemon as a bounded orchestration core

- **Status:** Accepted
- **Date:** 2026-10-03

## Context

After ADR-0065, the daemon owns lifecycle composition but holds 45 fields and
192 methods on one struct. Its size comes from forwarding methods, duplicate
start and runnability paths, multi-write child transitions with retries, and
parallel goroutine lifetimes. The orchestration flows call each other in cycles,
so splitting packages would add contracts without reducing their coupling.

## Decision

Keep the daemon in one package with state held by concrete types: a runner set,
tree locks, routes, a budget clock, and a lifetime. Use one start path and one
waiting queue. Make every subagent transition one `subagent.Store` transaction
covering the link and session rows. Have the controller read persistence,
progress, and the bus directly from their owners.

## Consequences

The daemon has no forwarding methods. Transient write failures are no longer
retried in process; atomic transactions remove the partial-transition gaps, and
boot recovers remaining failures. The daemon remains the largest core package,
at roughly 4.7k production lines after the refactor.

## Alternatives Considered

- Split the daemon into packages: rejected because its flows are cyclic and
  the split would require back-edge interfaces.
- Move subagent orchestration behind a host interface: rejected because the
  interface would need locks, starts, stops, tool retirement, publication, and
  wake operations with lock-order contracts.
- Keep retries: rejected because a transaction makes the gap they covered
  impossible.
