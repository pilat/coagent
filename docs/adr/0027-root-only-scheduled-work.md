# ADR-0027: Standalone scheduled work belongs only to root sessions

- **Status:** Accepted
- **Date:** 2026-08-22

## Context

The `schedule` tool created durable standalone work for whichever session
invoked it. A subagent can complete its current round and later be restarted by
one of those rows, without a new parent request or a matching subagent-link
obligation. Restricting an agent type's tool allowlist cannot close that path:
the built-in `general` type and project definitions with a wildcard deliberately
have broad ordinary access.

Schedules also remain valid future work after `/stop`. A due standalone
schedule addressed to a stopped root is a new root turn, but changing the
persisted status to active before its delivery claim would make a duplicate
retry reactivate work that SQLite already acknowledged.

## Decision

Standalone schedules are a root-session capability, determined from the
persisted `ParentID` rather than agent type. The daemon registers `schedule`
only for roots and rejects normal and fresh scheduled delivery addressed to a
subagent before runner creation or transcript mutation. `sleep` stays available
to subagents because its one-shot row owns the result of an existing suspended
call, not independent future work.

An invalid legacy subagent schedule occurrence is acknowledged as an unapplied
no-op. One-shots are removed and cron occurrences advance without a runner,
transcript mutation, publication, or retry. A standalone scheduled delivery may
construct a runner for a stopped root before its atomic delivery claim, but only
a successful claim activates the root and enters its agent loop; an
already-claimed retry leaves the root stopped.

## Consequences

- A completed subagent cannot be revived by work it scheduled in an earlier
  round, and new subagents never receive the capability to create that work.
- Legacy rows become inert without a migration or a retry/log storm.
- A stopped root still accepts due standalone schedule work exactly once, while
  stopped subagents and pending external-call results remain parked.
- An activation persistence failure after a successful claim can strand that
  occurrence before its answer; this is the accepted post-claim crash window.
- The root/subagent boundary is enforced at both capability registration and
  delivery, so broad agent-type allowlists cannot bypass it.

## Alternatives Considered

- **Rely on agent-type allowlists.** Rejected because wildcard project
  definitions and the built-in `general` subagent intentionally have broad
  ordinary tool access.
- **Remove `sleep` from subagents with `schedule`.** Rejected because a bounded
  sleep resolves an existing call and does not create autonomous future work.
- **Reject legacy rows as delivery errors.** Rejected because cron rows would
  retry and one-shots would consume the bounded failure budget despite being
  known-invalid work.
- **Reactivate a stopped root before claiming delivery.** Rejected because an
  idempotent retry could change persisted state without any new work to run.
