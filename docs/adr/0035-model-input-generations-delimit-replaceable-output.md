# ADR-0035: Model-input generations delimit replaceable output

- **Status:** Accepted
- **Date:** 2026-08-29

## Context

ADR-0030 lets consecutive replaceable outputs reuse the external message identities of the previous replaceable output. User input lives in a separate durable inbox, so the outbox previously could not see that the session had consumed a follow-up into a new LLM request. Telegram could therefore keep editing a progress card located above the consumed follow-up. Breaking on input enqueue is too early because pending input may wait behind unresolved work, and timestamps from independent transactions are not causal ordering.

The boundary is not limited to human input. Standalone scheduled turns also enter model context, but they bypass `session_inbox`. Compaction adds host-generated `role=user` rows that must not look like new work. The replacement contract therefore needs one durable signal shared by every real model-input path without merging inbox and outbox state machines.

## Decision

Each session carries a monotonic model-input generation and the transcript message ID at which that generation began. The session advances them atomically when ordinary inbox input is promoted or a standalone scheduled turn is injected. Pending input, host-handled commands, compaction, tool results, and external-call completions do not advance them. Several inputs promoted before one LLM request may advance the counter several times; that request's outputs all observe the final generation.

Every manager message output snapshots the current generation as host-owned outbox metadata in its insertion transaction. A manager may reuse previous external message identities only when the immediately preceding delivered message output is replaceable and both adjacent outputs carry the same generation. The outbox row order is authoritative: an output committed before generation advancement stays in the old chain even when its remote effect occurs later. Delivery does not re-read current session state or block input promotion on transport I/O.

Legacy adjacent outputs with no generation keep the legacy replacement behavior. A mixed legacy/current pair starts a new external message. Progress narration is scoped to active assistant tool-call text after the generation boundary, so a tool-only new turn cannot display narration from an older one.

Migration backfill places the boundary at the latest existing message rather than guessing which legacy input created the current turn. This fail-safe suppresses ambiguous old narration until the session stores new narration in a generated turn.

## Consequences

- A consumed follow-up or scheduled turn starts a new replaceable progress chain, while queued input leaves the current chain intact.
- Inbox and outbox remain separate ledgers with one small shared causal fact rather than one union state machine.
- Every message-output insertion path must inject the host generation, and progress identities must include it so source-key replay cannot cross a generation.
- Generation advancement and transcript insertion must share a transaction. A stale progress snapshot is discarded rather than stamped with a newer generation.
- Progress insertion also verifies that its captured root status remains eligible, so an older snapshot cannot publish `Working` after stop fences or parks the root.
- Delivery remains at-least-once. Once an outbox row commits, its generation and position define its causal order even if Telegram applies the effect later.
- The session schema gains durable generation metadata and old rows cause at most one fail-safe new message when mixed with generated output.

## Alternatives Considered

- **Break replacement when input is enqueued.** Rejected because queued input has not yet affected the model and may wait behind a pending external call.
- **Compare input and output timestamps.** Rejected because timestamps are captured in different transactions and do not prove commit order.
- **Merge inbox and outbox into one table.** Rejected because their consumers, retries, and terminal states remain different; a union table obscures both protocols to obtain ordering already supplied by a generation.
- **Use only `session_inbox.accepted_message_id`.** Rejected because scheduled model turns bypass the inbox.
- **Re-read the current generation when an output is claimed or delivered.** Rejected because it rewrites the meaning of an already-ordered outbox row and would require coupling model progress to remote transport latency for strict synchronization.
- **Use Telegram incoming message IDs.** Rejected because consumption, not external receipt, is the boundary and the daemon contract must remain transport-neutral.
