# ADR-0069: Manager routes own claims, publication and replacement

- **Status:** Accepted
- **Date:** 2026-09-28

## Context

Publication cached child/manager routing on the daemon receiver, while
attribute claims and root replacement updated related durable ownership under
another lock on the same receiver. A stale publication read must lose to a
committed claim. A replacement must retain ownership fencing through retirement
of the old root. Extracting only cache access would split both guarantees.

## Decision

A private manager-route owner in daemon holds publication caches, claim and
replacement serialization, attribute policy and owner-aware root replacement.
It consumes durable route/manager-root capabilities, project metadata and the
existing event bus. Daemon remains the application and lifecycle coordinator.

The replacement caller holds the tree fence. The route owner performs the full
replacement, notification and old-root retirement under its ownership lock,
using a narrow lifecycle effect for retirement. Publication uses the separate
cache lock so notifications during retirement cannot reacquire the ownership
lock. Existing durable replacement transactions remain unchanged.

## Consequences

Owner claims and cache updates cannot be maintained independently by daemon
callers. Root and child publication rules remain in the same component as the
ownership transitions they depend on. One lifecycle callback is deliberate:
root retirement belongs to lifecycle while its placement belongs to replacement.

The tree, ownership and cache lock order remains a cross-component contract.
The owner does not acquire runner, schedule or general input-routing policy.

## Alternatives Considered

- **Extract the publication cache alone.** Leaves ownership policy and stale-read
  arbitration split between daemon and the cache wrapper.
- **Return replacement state for daemon to finish.** Releases the ownership
  fence before the complete transition and obscures the late-claim guarantee.
- **Move general daemon operations into the route owner.** Recreates the broad
  application coordinator rather than isolating manager ownership.
