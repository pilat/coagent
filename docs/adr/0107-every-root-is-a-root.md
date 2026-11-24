# ADR-0107: Every root is a root

- **Status:** Accepted
- **Date:** 2026-11-24
- **Amends:** [ADR-0106](0106-one-operator-every-surface-reaches-every-root.md)
- **Amends:** [ADR-0030](0030-durable-session-outbox.md)
- **Amends:** [ADR-0032](0032-tools-return-host-owned-persistent-output.md)
- **Amends:** [ADR-0036](0036-explicit-stop-has-a-durable-terminal-output.md)
- **Amends:** [ADR-0072](0072-flat-chat-with-a-focus.md)
- **Amends:** [ADR-0073](0073-chat-shows-where-each-message-comes-from-and-every-model-step.md)
- **Amends:** [ADR-0093](0093-one-turn-grants-and-config-apply-live-in-the-process.md)
- **Amends:** [ADR-0097](0097-orchestrator-mode-is-a-human-granted-capability-on-an-ordinary-root.md)
- **Amends:** [ADR-0101](0101-an-orchestrator-ends-only-sessions-it-spawned.md)
- **Amends:** [ADR-0104](0104-the-local-api-is-the-two-durable-ledgers.md)

## Context

[ADR-0106](0106-one-operator-every-surface-reaches-every-root.md) removed per-manager ownership but kept `manager_id`
as a marker: only a root carrying it had an output log, the config tools and orchestrator eligibility. Every root
created in production carries it, through a manager, an API client or a spawn. The roots without it were legacy
rows from before [ADR-0023](0023-manager-owned-session-publication.md), and test fixtures. Several decisions still
named the split:
- ADR-0030: "Outbox/inbox rows also carry JSON attributes with an immutable `manager_id`".
- ADR-0032: "Ownerless roots and subagents keep only the model-facing …".
- ADR-0036: "ownerless/direct stop … retain their separate output paths".
- ADR-0072: "Management tools live on every manager-owned root".
- ADR-0073: "Every accepted tool-calling response of a manager-owned root writes one output-log message under the
  existing `message:<id>:reply` key:".
- ADR-0093: the one-turn grant is created only for "a user-source input with a non-empty manager identity".
- ADR-0097: "Orchestrator mode is a durable `orchestrator: true` attribute on an ordinary manager-owned root."
- ADR-0101: "The only inputless stop is the ownerless one, which writes no output-log rows and has no boot recovery;
  copying the explicit stop without its row duplicates the flow."
- ADR-0104: "Since then both directions became durable ledgers: every input enters `session_inbox` before anyone acts
  on it, and every user-visible output of a manager-owned root is an append-only `session_outbox` row with one
  table-wide AUTOINCREMENT id ([ADR-0030](0030-durable-session-outbox.md),
  [ADR-0072](0072-flat-chat-with-a-focus.md))."

The marker was the last piece of tenancy in the core. It kept a second kind of root alive with
its own `/clear`, stop and output paths.

## Decision

- Every root (`parent_id = 0`) is a root. It has an output log, titles, episodes, step messages with the tool
  trail, the config tools behind `/config` and orchestrator eligibility. Nothing stamps, copies or reads
  `manager_id`. Existing rows keep it unread.
- Root-ness is `parent_id = 0`, everywhere it is checked. Subagents stay internal: they get no output rows and no
  manager events, and their lifecycle inputs keep the silent path.
- A one-turn grant or skill receipt is created for a user-source input on a root, decided by the session's own
  root-ness and never by an input attribute.
- The bare root row survives only as a test fixture in `internal/sessionstore/sessionstoretest`, which no binary
  imports.

## Consequences

- One kind of root, one `/clear` path, one stop path and one output path.
- Accepted: legacy roots without the marker appear on every surface and gain everything a root has, including
  config tools, the `/config` grant and orchestrator eligibility.
- Accepted: the `CreateManagerRoot`, `ReplaceManagerRoot` family keeps its historical names and "manager root"
  wording in comments and error strings until a rename-only follow-up.
- Accepted: a `manager_id` a caller might pass is stored and read by nothing; no production caller passes one.
- Accepted: a legacy root has no `session_opened` row or earlier output-log history; chat entry and `TailOutput` show nothing until its next row, and nothing backfills old rows.
- Accepted: `errSessionNotFound` now wraps `sql.ErrNoRows`, so a session missing at the process-tree fence lookup in `internal/daemon/trees.go` reads as fenced instead of a lookup error.
- Accepted: a markerless root left mid-`/clear` by the old ownerless path at upgrade, terminating with no replacement row, is killed at boot with a `session_closed` row.

## Alternatives Considered

- **Keep the marker.** It preserves a second kind of root that exists only in legacy rows and tests.
- **A migration that stamps or kills legacy roots.** The owner wants them to appear and behave like every root.
  Stamping would keep a value nothing reads, and killing would lose sessions.
- **Keep `Store.CreateSession` as a production method.** It would have no production caller, and the deadcode gate
  forbids that. A copy of it in a `_test.go` file beside a cross-package fixture would be two implementations of one
  row.
