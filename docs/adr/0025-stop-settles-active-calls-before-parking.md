# ADR-0025: Stop settles active calls before parking a session tree

- **Status:** Accepted
- **Date:** 2026-08-21

## Context

Coagent resumes unfinished sessions after a daemon restart. That is correct for
ordinary interruption, but it makes `/stop` correctness depend on the durable
transcript: every active `tool_use` must have a result before the session can be
parked. Stop cleanup previously settled only producer-owned external calls. If
cancellation interrupted persistence of an ordinary Bash result, the assistant
turn remained incomplete and a later message or restart executed it again.

Stopping a tree also races independent writers. Joining the session runner
excludes runner-owned writes, but child completion, schedules, and config
verdicts use their own durable producer paths. A stopped blocking task must not
be completed by an old child result that wins this race, because explicit child
reuse creates a new activation and a new parent obligation.

## Decision

`/stop` is a durable two-phase park. The daemon marks the active root tree and
links as stopping, snapshots its live runners, cancels all runners immediately
without a grace period, and then joins them. After runner exclusion and producer
fencing, it appends `Stopped by user.` exactly once for the union of globally
pending external calls and unresolved ordinary calls in each current active
assistant turn. It then completes existing inbox, sleep, and link cleanup and
marks the sessions stopped.

The stop-specific session operation may settle calls without an external
producer, but only at this lifecycle boundary. Normal external completion keeps
its exact identity and producer-ownership checks. Historical ordinary calls
superseded by later turns retain existing transcript projection and repair
semantics; stop does not revive them.

The initial stopping notification is immediate, but durable cleanup remains
synchronous and has no artificial delay or timeout. The final stopped state is
published only after settlement completes. Startup extends the existing
synchronous recovery for `stopping` trees and finishes the same private,
idempotent cleanup before orphan resolution or automatic continuation; recovery
does not republish a new user-command progress message.

If cleanup fails, the command returns the error and does not publish the final
stopped state. Unfinished sessions remain `stopping`; startup recovers each
topmost remaining stopping subtree before normal continuation. Stop covers only
active descendants at its boundary and does not rewrite historical terminal
descendants.

A new user message resumes only the stopped root as fresh work. A stopped child
resumes only through explicit follow-up, which creates a new activation and is
the only path by which its later result becomes visible to the root. The old
blocking `task` call remains stopped.

## Consequences

- Explicitly stopped work cannot be replayed by a later message or restart,
  while sessions interrupted without `/stop` keep automatic continuation.
- Stop latency is cancellation, join, and necessary durable cleanup; no runner
  receives extra time to finish voluntarily.
- Correctness depends on both runner join and existing link/status/producer CAS
  fences. Tests must exercise late child completion, sleep, config verdict, and
  ordinary result persistence at each durable stop boundary.
- An interrupted stop needs no new table: session status, append-only messages,
  subagent links, inbox rows, and schedule ownership contain the recovery facts.
- The session interface gains a powerful stop-only operation. Its narrow naming,
  call sites, and tests must keep it out of normal completion paths.

## Alternatives Considered

- **Settle only external calls.** Rejected because an interrupted ordinary tool
  call caused the production replay that motivated this decision.
- **Treat every historical dangling ordinary call as active.** Rejected because
  later turns deliberately supersede ordinary calls; reviving them changes
  transcript projection semantics and may produce invalid provider ordering.
- **Return before durable settlement and finish asynchronously.** Rejected
  because a reported stopped state would not yet guarantee non-replay.
- **Give tools a grace period or add a cleanup timeout.** Rejected because stop
  should cancel immediately, while abandoning settlement on timeout weakens the
  durable guarantee.
- **Persist a separate stop ledger.** Rejected because existing durable status,
  transcript, links, inbox, and schedules already identify the required work.
