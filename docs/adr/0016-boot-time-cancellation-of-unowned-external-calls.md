# ADR-0016: An external call with no surviving producer is cancelled at boot

- **Status:** Accepted
- **Date:** 2026-08-17

## Context

A pending external call is detected two different ways. The loop's deferral check
is **ledger-keyed** (`CreateOptions.StagedExternalCalls`, merged from the staged
ledger, sleep schedules, blocking child links and the pending-apply marker);
transcript repair's exclusion is **name-keyed** (`tool.IsExternalCall`). The widths
differ on purpose: repair over-protects because stubbing a live call loses a
verdict, while the loop only blocks on calls a producer says have actually started.

The staged ledger is deliberately in-memory — a daemon that died before recording
work never did it, so re-executing is correct. But `request_secret` records nothing
else. After a restart the transcript still shows its `tool_use`, so repair refuses
to stub it, while no ledger owns it, so the loop happily appends the next user
message and ships an assistant turn whose `tool_use` has no `tool_result`. The
provider rejects that request, and every later one: the session is bricked
permanently. The same state is reachable for a config apply whose pending-apply
marker did not survive.

So the two sets must agree by the time a session runs, and only the daemon knows
enough to make them agree.

## Decision

We add PASS 0 to the startup sweep (`daemon.resolveOrphanedCalls`). For every
session that can still ship its transcript (active, suspended or error; never
killed or parked), it compares the name-keyed pending set read from the durable
transcript against the merged producer ledgers, and answers the difference with a
deliberate cancellation — for `request_secret`, "the terminal prompt was lost (the
daemon restarted); ask again if the secret is still needed".

This is an **owned cancellation, not a repair stub**: the daemon adopts the call
into the staged ledger first, so the normal `ResolvePendingCall` contract (exact id,
matching tool name, idempotent) still validates the answer. The session is opened
without a runner and closed again, so recovery neither burns an LLM call nor
re-asks a question no terminal is waiting for. The model sees the cancellation the
next time the session runs and may simply re-issue the call.

The check is generic over `tool.IsExternalCall`, so any external call that loses
its producer is covered, not just the one that motivated it.

## Consequences

- The invariant now holds across restarts: the ledger-keyed and name-keyed pending
  sets agree, and every pending external call has an owner able to resolve it.
- A masked prompt abandoned by a restart costs the user one extra exchange instead
  of the session.
- The answer is a cancellation, so the outcome of the lost work is genuinely
  unknown to the model; the generic notice says so and tells it to check state
  before retrying. A config apply whose marker vanished may already be live.
- Boot pays one transcript read per active/suspended/error session, and pays it
  before `Start` returns: managers and the schedule executor come up next, and a
  runner either of them opens makes the pass skip that session silently.
- Sessions that are finished, parked or killed are skipped. A parked one is still
  resumable, so `/stop`'s own settle reads the same durable set (a stop whose
  second phase runs after a restart owns nothing in memory either).
- A producer added later that suspends a call without recording it durably will now
  have its call cancelled at the next boot instead of bricking the session. That is
  a better failure, not a licence: `ErrSuspend` still requires a recorded owner.

## Alternatives Considered

- **Make `request_secret` durable (a new table or a marker file).** Re-attach does
  re-emit outstanding requests, but only from the in-memory registry, which dies
  with the process — so after a restart there is nothing left to replay to the
  terminal that asked. Persisting the question means persisting the registry too,
  and paying a migration for a prompt whose asker is gone. Cancellation is the
  honest semantics; a durable prompt queue is a separate feature with a
  controller half.
- **Let repair stub unowned external calls (narrow the wide set).** One line, and
  it removes the deliberate asymmetry that keeps a live verdict from being
  overwritten by "[transcript repair] missing tool result". A missing ledger entry
  would silently become a fabricated answer.
- **Resolve the call by reviving the session and letting the loop run.** Every
  orphan would cost an LLM call at boot and could re-issue a prompt into an empty
  terminal. The transcript only has to be *valid* at boot; it does not have to
  advance.
