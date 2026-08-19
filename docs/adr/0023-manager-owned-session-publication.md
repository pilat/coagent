# ADR-0023: Session events are routed to one owning manager

- **Status:** Accepted
- **Date:** 2026-08-19

## Context

The controller previously exposed `SubscribeAll`, so every manager received every
root-session event. Telegram treated each `session.created` event as its own and
created a forum topic for it. A conversation started by the local CLI therefore
appeared in Telegram, including the model's replies. A local filter in Telegram
would fix that symptom but would leave every future manager responsible for
reimplementing the isolation rule. A transport label such as `channel=telegram`
also cannot distinguish several configured Telegram managers.

## Decision

- Every manager-created root session has one durable owner: the manager's unique
  config ID, stored as the reserved `manager_id` session attribute. The
  composition root binds a separate private controller capability to each ID;
  session creation stamps it rather than accepting it as caller input.
- Every session-addressed controller read or mutation verifies that durable
  owner before dispatch. The subscription also accepts only the controller's
  bound ID. The daemon publication gate fans an event out only to subscribers
  for that exact ID; ownerless sessions and failed owner lookups reach none.
- Owner identity is immutable once assigned. Attribute updates preserve it,
  concurrent claims are serialized, clear/recreation copies it, and publication
  cache updates cannot overwrite a newer claim with a stale lookup.
- Manager-local ownership checks remain as defense in depth for reconciliation,
  lookup and command listings. The CLI may claim an ownerless session only in
  its reserved project with the old `channel=cli` marker. Telegram does not
  claim ownerless sessions: topic IDs are scoped to a chat and do not uniquely
  identify one of several configured managers. Other ownerless sessions remain
  invisible.
- The built-in `cli` manager ID is reserved and rejected in configured manager
  entries, so a configured transport cannot subscribe to local-chat events.
- Daemon-internal per-session and observer subscriptions remain available for
  lifecycle coordination and test traces; they are not part of the manager
  controller contract.

## Consequences

- Adding another manager does not require trusting its notification handler to
  ignore foreign conversations or authorize every command independently: the
  shared manager-bound controller boundary is fail-closed in both directions.
- Several managers using the same driver remain isolated because ownership uses
  the configured manager ID, not the driver or channel name.
- Existing CLI conversations can be adopted without a schema migration because
  their reserved project and old channel marker jointly prove ownership. Legacy
  ownerless Telegram conversations are not resumed automatically; ambiguous
  recovery is less safe than failing closed.
- A configured manager cannot reuse the reserved built-in `cli` identity; the
  runtime records that manager's start error and keeps the local chat isolated.

## Alternatives Considered

- **Filter only in Telegram.** Rejected because every future manager could repeat
  the same leak and non-notification paths such as startup reconciliation would
  still need separate guards.
- **Route by transport channel or driver.** Rejected because two Telegram
  managers share both values but do not own each other's sessions.
- **Store ownership only in memory.** Rejected because restart and clear are
  normal lifecycle transitions; routing identity must survive both.
