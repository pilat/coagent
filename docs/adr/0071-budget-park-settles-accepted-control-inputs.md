# ADR-0071: Budget parking settles accepted control inputs behind the stop fence

- **Status:** Accepted
- **Date:** 2026-09-29

## Context

A fired budget ends the current session activation. If the terminal output for
an accepted `/compact` fails to commit, the command stays pending while its
in-memory retry state disappears with that activation. Tree-stop cleanup then
cancels pending user inputs, so the command can lose its answer. The tree lock
does not guard inbox admission: a command may also arrive after an unfenced
pre-stop scan and before cleanup.

## Decision

Budget parking settles accepted `/compact` inputs after the stop operation has
marked every planned session `stopping` and joined its runners, but before it
cancels remaining pending inputs. The persisted `stopping` status fences new
inbox admission. Settlement scans all pending user inputs in the planned tree,
not only the FIFO head, and uses the same parked notice and output identity as
the live command. Manager-owned roots commit the handled input and outbox row in
one transaction; other sessions resolve the input without a manager outbox row,
matching their existing notification boundary.

A settlement failure aborts stop cleanup before input cancellation or the
parked budget marker. The pending input and draining budget remain durable, so
the existing budget reconciler retries the park. On daemon startup, a stopping
tree with a pending budget park is reserved for that park's recovery pass;
generic interrupted-stop recovery must not cancel its command first. Ordinary
explicit stops keep their existing input-cancellation behavior.

## Consequences

An accepted managed-root `/compact` receives one durable parked output even if
its live activation loses a terminal write or a normal pending message is ahead
of it. Other sessions retain one durable handled-input fact. No schema
migration or new retry ledger is needed; the pending inbox row is the retry
intent, and command settlement is already atomic. The stop protocol gains one
optional control-input phase with an explicit ordering requirement.

## Alternatives Considered

- **Retry only inside the session.** The budget fire ends that activation, so
  its in-memory retry cannot survive the park boundary.
- **Scan before beginning the stop.** Inbox admission can race between the scan
  and the durable `stopping` fence.
- **Preserve pending commands after the stop.** This needs a second recovery
  path for stopped sessions and leaves the command unanswered until it runs.
