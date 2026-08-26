# ADR-0032: Tools return host-owned persistent output

- **Status:** Accepted
- **Date:** 2026-08-26

## Context

A tool mutation may need an authoritative user-facing receipt even when model
prose is missing, inaccurate or the process crashes before the next assistant
turn. Making each such tool import Telegram, CLI or daemon delivery code would
violate package boundaries and duplicate the manager-owned durable outbox.

The same need is not specific to budgets. A protocol-level result should support
direct user output for any synchronous tool, while preserving the distinction
between message durability and whether a session is ready for more input.

## Decision

`tool.Result` may carry bounded direct user messages alongside its model-facing
result. Direct messages are always persistent output; tools cannot choose
replaceable behavior, manager identity, delivery receipt, transport attributes
or lifecycle readiness.

For a synchronous tool, the host validates the complete direct-output set and
commits its tool-result transcript row and persistent outbox rows in one
transaction. Deterministic source identities make replay idempotent. The outer
executor preserves tool-call order, and `batch` propagates nested direct output
in nested call order. Ownerless roots and subagents keep only the model-facing
result and do not publish direct output.

Input readiness is a separate host-owned property. A persistent direct message
does not by itself announce idle or release a manager prompt. A host lifecycle
output is marked as a readiness candidate, and the daemon announces readiness
only after both that exact output is delivered and its terminal or parked state
is current.

## Consequences

- A tool can produce an authoritative receipt without relying on a later model
  response.
- All user-facing delivery remains manager-owned, durable and replayable.
- The generic tool protocol grows a user-output vocabulary but remains unaware
  of managers and transports.
- Direct outputs must be bounded, validated and secret-redacted before any part
  of the transaction commits.
- Suspending and externally completed tools retain their producer-specific
  ledgers; this synchronous transaction does not replace those protocols.
- Persistent output and session readiness can no longer be treated as the same
  boolean by CLI or other managers.

## Alternatives Considered

- **Let the model repeat the tool result to the user.** Rejected because the
  model may omit or alter the receipt and a crash can strand a committed side
  effect without confirmation.
- **Add budget-specific daemon messaging.** Rejected because the requirement is
  generic and special delivery paths would bypass the outbox contract.
- **Let tools publish directly to managers.** Rejected because tools do not own
  manager routing, durable receipts or transport retry semantics.
- **Let each tool choose persistent or replaceable output.** Rejected because
  replacement and lifecycle semantics belong to the host and manager protocol,
  not an implementation leaf.
- **Treat every persistent direct output as idle.** Rejected because receipts
  can occur in the middle of an active tool-calling turn.
