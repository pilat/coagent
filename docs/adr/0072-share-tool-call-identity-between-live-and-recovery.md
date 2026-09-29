# ADR-0072: Share call ownership and scope durable results to one invocation

- **Status:** Accepted
- **Date:** 2026-09-29

## Context

Live sessions and daemon recovery independently identified unresolved tool calls.
A temporary transcript session inside the live session added forwarding without
owning a distinct transition. Both paths used the provider's call ID to match
results, while durable result replay searched every message in the session by
that ID. Compaction hides old calls from the model but retains their rows. If a
later call reused an ID, execution could succeed while its result replayed or
conflicted with the older row.

## Decision

`internal/sessioncalls` owns the shared active-transcript classifier and
settlement rules for live execution and store-backed recovery. A completed
call may be followed by another call with the same provider ID. An overlapping
unresolved call or two equal IDs in one assistant response cannot be paired
unambiguously and is rejected before tool execution. The paid invalid attempt
is retained outside active model context; a bounded host retry follows unless
the budget checkpoint fired.

Durable result identity is the saved assistant message row ID plus the call's
index in that message, not the provider ID. `InsertToolResultSetOnce` validates
the referenced call and deduplicates its result and direct output by this
identity. Provider IDs remain unchanged in model-visible calls and results.
Recovery resolves only pending invocations and writes an interrupted outcome
without rerunning a tool. Migration 46 adds nullable owner/index columns and a
unique index for new results; old rows remain intact and can replay only when
their owner is unambiguous.

## Consequences

Daemon retains producer claims, startup ordering and lifecycle fences; session
retains its agent loop and compaction. External calls remain causal pending
state, while interrupted in-loop recovery considers only the latest assistant
turn. A completed historical subagent link cannot block a new invocation that
reuses its provider ID. Ambiguous legacy rows fail closed rather than being
assigned to an arbitrary invocation. No historical message is deleted.

## Alternatives Considered

- **Session-wide uniqueness of provider IDs.** The result store would continue
  to confuse a new invocation with retained compacted history.
- **Restrict result lookup to active rows.** A crash retry would lose its
  durable idempotency key and direct output or subagent links could still
  collide with historical rows.
- **Rewrite provider IDs.** Providers require the original call/result ID pair;
  rewriting signed or opaque response content is not a portable contract.
- **Keep the separate recovery decoder with a duplicate check.** Leaves two
  definitions of pending calls and their turn scope to drift again.
