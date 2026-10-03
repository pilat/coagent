# ADR-0065: Daemon owns lifecycle composition

- **Status:** Accepted
- **Date:** 2026-10-03

## Context

The session loop now commits each step through one transaction and reads its
durable inbox directly. Protocol owners deliver through that inbox. Separating
runner lifecycle from the daemon therefore leaves one implementation on each
side of interfaces that forward back into daemon callbacks for launch,
completion, publication and stop effects. Temporal ordering spans both packages
without providing independent lifecycle behavior.

This supersedes ADR-0038's separate lifecycle component and store-facet grouping.
Its manager-control boundary and provider-neutral transcript identity remain.

## Decision

The daemon owns concrete runner, registry, admission-queue, recovery and tree
state. It calls protocol owners directly and contains no SQL or tool
implementations. Session-store owns project SQL alongside session persistence;
session, daemon and manager-control declare their own consumer persistence views.

The daemon pushes live state into progress runtime, which publishes through the
shared session bus. Wall-time budget timers belong to the daemon. Failed
observations and unfinished parks remain scheduled; each park checks its durable
generation and owner under the tree fence before changing the tree.

Stop fences links before cancellation, joins runners, settles calls and
activations, finishes statuses, then cancels producer obligations. The explicit
root's terminal output and stopped status commit atomically. Boot completes
lifecycle recovery and budget reconciliation before orphan and interrupted-call
settlement enables input waking and session recovery.

## Consequences

Lifecycle ordering is visible in the package that controls it. Single-purpose
forwarding interfaces and callbacks disappear, while durable transaction and
producer ownership remain separate. Runner registration shares a live-state
fence with idle publication, so a controller cannot observe idle while a runner
is live.

The daemon contains more lifecycle code and must keep its responsibility-based
files bounded. Protocol owners receive the concrete session ledger where they
need inbox transactions; they no longer have independently named store facets.

## Alternatives Considered

- **Keep the lifecycle package and forwarding callbacks.** Rejected because
  it splits one temporal protocol across packages without an independent owner.
- **Introduce another lifecycle coordinator or event bus.** Rejected because
  it adds indirection to ordering already owned by the daemon and durable inbox.
- **Move SQL or tools into the daemon.** Rejected because composition must not
  acquire persistence or protocol implementation ownership.
