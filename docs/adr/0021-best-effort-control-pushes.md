# ADR-0021: Keep control pushes best effort and make overflow observable

- **Status:** Superseded by [ADR-0030](0030-durable-session-outbox.md)
- **Date:** 2026-08-18

## Context

Control-socket replies and server pushes share one client read loop. Blocking
that loop on an unread notification consumer would stall unrelated RPC calls and
can deadlock local chat. An unbounded queue merely moves that failure into memory
growth. The durable transcript, not pushes, is the recovery source after a
client reconnects.

## Decision

Each `ctl.Client` keeps a bounded notification channel. When it is full, the
read loop drops the push and continues. The client counts those channel-overflow
drops atomically and exposes the count through `DroppedNotifications`.

## Consequences

An unread push stream cannot block replies, and internal callers can tell that
their local view lost events. The counter is connection-local, not durable and
not user-facing telemetry. There is no replay, sequence number or automatic
resynchronization protocol; callers that require authoritative state must query
the durable source after reconnecting.

## Alternatives Considered

- **Block until a consumer reads every push.** Rejected because it makes a
  stalled UI able to stop every call on the same connection.
- **Use an unbounded queue.** Rejected because a stuck client can consume
  unbounded daemon memory.
- **Add replay and resynchronization now.** Rejected because it needs sequence,
  retention and client recovery contracts beyond this release's scope.
