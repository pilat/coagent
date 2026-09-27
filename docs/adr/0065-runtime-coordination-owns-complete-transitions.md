# ADR-0065: Runtime coordination owns complete transitions

- **Status:** Accepted
- **Date:** 2026-09-27

## Context

ADR-0038 introduced explicit runtime boundaries, but daemon still coordinated
registry mutation, admission release, overflow queues and tree-stop ordering
outside those boundaries. The lifecycle package protected individual mechanisms
while callers retained the decisions connecting them. Process and resource
dependencies were also discovered through concrete implementation assertions.

Response handling had a similar split: executable sessions could select a
legacy persistence path for lightweight tests, and child completion recovered
answer meaning separately from the durable session evidence. Progress
reconciliation additionally controlled budget deadlines, tying presentation
cadence to execution control.

## Decision

Complete ownership boundaries around invariants, retaining the existing
process topology, database schema and atomic transitions.

- A lifecycle supervisor owns runner registration, admission release, overflow
  queues, queue retry lifetime and tree fencing. Tree-stop coordination uses
  explicit producer and transcript effects while retaining the same stop and
  recovery phases. Cancellation completion remains separate from finalization
  that needs the tree fence.
- Construction supplies process storage and a shared tool-resource owner
  explicitly. Each executable session receives its process service directly.
  The factory receives stack access; daemon receives only the neutral resource
  lifecycle contract defined alongside the tool protocols.
  Transcript settlement has a separate capability and creates no model client
  or tools.
- Executable sessions use the accepted-response disposition transaction. Pure
  response classification is independently testable, but no missing capability
  selects a weaker production algorithm. Recovered activation outcomes have
  one projection over existing durable facts.
- Cross-table child completion receives the canonical completion-check
  invalidator at construction and invokes it inside its delivery transaction.
  No second SQL implementation or mutable installation step remains.
- Lifecycle reconciliation owns budget deadlines and startup observation;
  progress consumes committed facts. The existing budget transaction remains
  the authority for current cost, duration crossing and generation fencing.

This extends ADR-0038's direction rather than replacing its store, manager or
input ownership decisions. Producer-specific integration remains in daemon;
moving it wholesale would create another oversized coordinator.

## Consequences

Runtime consumers no longer manipulate the supervisor's registry and admission
counter independently. Required resource capabilities survive factory wrappers.
Tests that execute a session exercise the same response transaction contract as
production; transcript and policy-only tests remain independently testable.

The refactor requires adapting construction fixtures and temporal test scripts,
including candidate/confirmation turns that old memory-only tests omitted.
The supervisor and budget reconciler introduce explicit lifetime contracts that
must be stopped and joined. They add no durable ledger and do not change
external delivery from at least once to exactly once.

## Alternatives Considered

- **Move files and retain daemon state access.** Rejected because it would keep
  transition ordering distributed while increasing navigation cost.
- **Split persistence by table.** Rejected because response, input, output and
  child delivery invariants require cross-table atomicity.
- **Keep a lighter executable algorithm for unit tests.** Rejected because its
  behavior differs from the protocol the tests are intended to protect.
- **Introduce a generic event/workflow framework.** Rejected because the
  existing durable ledgers already express recovery; another framework would
  duplicate authority and complicate migration.
- **Keep deadlines in progress reconciliation.** Rejected because rendering
  failure or cadence changes should not control budget enforcement.
