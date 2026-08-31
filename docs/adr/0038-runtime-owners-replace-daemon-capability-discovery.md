# ADR-0038: Runtime owners replace daemon capability discovery

- **Status:** Accepted
- **Date:** 2026-08-31

## Context

The daemon had accumulated unrelated ownership: active-runner maps and mutexes,
admission queues, recovery goroutines, stop fencing, child completion ordering,
manager authorization and controller DTO conversion. Session and daemon code
also discovered transactional session-store capabilities through runtime type
assertions. The interfaces looked narrow at their declarations, but the real
dependencies were hidden and missing capabilities silently selected weaker
non-atomic paths.

Persistence vocabulary leaked in the opposite direction too: provider-facing
messages carried database row IDs, and progress runtime imported the complete
session package for a four-field context projection. These edges made the large
packages difficult to split without changing temporal behavior.

## Decision

Runtime coordination receives explicit owners and compile-time contracts.

- `sessionlifecycle` owns runner state and registration, launch/admission
  arbitration, FIFO overflow caches, the recovery worker, durable stop phases,
  and child terminalization/delivery ordering. Daemon callbacks retain session
  assembly, tool/schedule effects and other external integration work between
  those owned phases.
- `managercontrol` implements the manager-bound controller application use
  cases over a daemon backend; `managerdiscovery` owns project, model, skill and
  filesystem discovery. The composition root binds these services directly.
- `inputruntime` implements durable FIFO promotion, one-turn activation and
  atomic command output at the session boundary.
- Session-store consumers receive named owner contracts for agent runtime,
  manager output, manager-root transitions and lifecycle settlement. The
  complete `sessionstore.Store` remains a constructor result and test fixture,
  not a runtime dependency.
- Provider-neutral messages contain no persistence identity. Session carries a
  positional row-ID sidecar across reload and compaction, while `progress`
  owns the neutral context projection shared with `progressruntime`.
- `.go-arch-lint.yml` records every new package and permitted dependency edge;
  removed reverse edges are not retained as compatibility allowances.

## Consequences

Session and daemon perform no runtime session-store capability assertions.
Missing atomic persistence or output dependencies fail at compile or
construction time. Runner append/teardown, registration/shutdown, stop/spawn
and completion/re-arm races have one synchronization owner and remain covered
by temporal scenario and race tests.

The daemon remains the integration backend and still contains sizeable session
assembly and producer-specific recovery callbacks. Those callbacks are explicit
ports rather than shared mutable state, so later package extraction does not
require moving the lifecycle state machine again.

Construction names more role interfaces over the same SQLite implementation.
That repetition is deliberate: collapsing them into a dependency bag or the
complete store would recreate hidden coupling.

## Alternatives Considered

- **Keep optional type assertions for narrow tests.** Rejected because tests
  selected weaker behavior than production and could not prove the atomic path
  was actually wired.
- **Move files but leave mutexes and maps on daemon.** Rejected because package
  names would change without moving concurrency ownership or reducing races.
- **Split session-store by table.** Rejected because inbox, transcript, outbox,
  budget and lifecycle rows participate in named cross-table invariants. The
  transaction, not the table, determines the owner boundary.
- **Put controller logic on managers.** Rejected because authorization and
  durable output semantics must be identical across built-in transports.
- **Create one generic runtime dependency bag.** Rejected because it would hide
  the exact lifecycle and transaction capabilities this decision makes
  reviewable and mechanically enforceable.
