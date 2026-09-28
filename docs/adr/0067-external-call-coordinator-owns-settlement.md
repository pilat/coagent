# ADR-0067: External-call coordinator owns producer claims and settlement

- **Status:** Accepted
- **Date:** 2026-09-28

## Context

The daemon coordinated staged calls, config application, activation settlement
and transcript recovery through a shared receiver. A staged call is broader
than a config change: stop and restart recovery temporarily adopt external
calls so transcript settlement recognizes an owner. Extracting only config
methods would leave ownership transitions divided across the same callers.

## Decision

A private coordinator in daemon owns staged producer claims, the merged
pending-call projection, config handoff and per-session transcript settlement.
Its dependencies are supplied at construction; it receives no daemon receiver.
Daemon retains startup pass ordering, tree fences, runner lifecycle and input
delivery, and invokes complete coordinator operations at those boundaries.

Normal result delivery retires ownership after successful transcript resolution.
Temporary orphan adoption is always released on return, including failure, so
future recovery can still recognize the orphan. Unbacked config changes retain
their existing absent-call and shutdown cleanup. Abandonment releases the apply
slot before teardown; grant expiry and transcript settlement remain behind the
caller's writer fence.

This preserves the policies of [ADR-0015](0015-one-apply-in-flight-one-marker-consumption.md),
[ADR-0016](0016-boot-time-cancellation-of-unowned-external-calls.md) and
[ADR-0059](0059-pending-tool-calls-are-never-reexecuted-on-resume.md).
The coordinator consumes existing producer ledgers and adds no durable state.

## Consequences

The daemon no longer mutates staged state or implements config grant transitions
while assembling runners. Recovery still opens only transcript capabilities.
Typed delivery effects connect the coordinator to existing input routing;
claim, marker and settlement decisions remain inside the coordinator.

Startup ordering and fences remain explicit caller obligations. This component
must not grow to own schedules, child lifecycles or generic input routing.

## Alternatives Considered

- **Extract only config tools.** Leaves generic orphan/stop adoption and config
  ownership retirement with separate mutable owners.
- **Move all recovery and routing into the coordinator.** Mixes producer-claim
  settlement with global lifecycle and admission decisions.
- **Introduce a new durable call ledger.** Duplicates existing transcript,
  schedule, child-link and config-marker authorities without a behavior need.
