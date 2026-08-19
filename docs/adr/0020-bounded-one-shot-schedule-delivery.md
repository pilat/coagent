# ADR-0020: Bound one-shot schedule delivery retries per executor

- **Status:** Accepted
- **Date:** 2026-08-18

## Context

A one-shot schedule can become permanently undeliverable when its session has
been removed or its pending call no longer exists. Retrying it on every executor
tick forever consumes work and logs without creating a recovery path. Durable
dead-letter state would require a migration and a user-visible management flow,
neither of which exists for this release.

## Decision

The executor keeps an in-memory consecutive-failure count for each one-shot
schedule. It removes the schedule after ten failed delivery attempts. A success,
including an idempotent already-applied=false acknowledgement, removes the row
and clears its count. Creating a new executor resets all counts.

## Consequences

An unavailable one-shot cannot cause an unbounded retry storm, but a process
restart gives it a fresh ten-attempt budget. Delivery failures are not durable
and there is no dead-letter record or operator recovery action. The durable
schedule row remains the source of truth until it is accepted or discarded.

## Alternatives Considered

- **Retry forever.** Rejected because a deleted session creates permanent work
  and log noise.
- **Persist retry state and dead letters.** Rejected for this release because it
  changes the schema and needs a user-visible inspection and recovery contract.
- **Drop on the first error.** Rejected because transient daemon and SQLite
  recovery windows would lose a valid wake too readily.
