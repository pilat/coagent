# ADR-0093: One-turn grants and config apply live in the process

- **Status:** Accepted
- **Date:** 2026-11-14
- **Amends:** [ADR-0031](0031-user-commands-grant-one-turn-tool-authority.md), [ADR-0057](0057-reuse-user-command-activation-for-config-edits.md), [ADR-0015](0015-one-apply-in-flight-one-marker-consumption.md)

## Context

The reliability-diet inventory found two protocols guarding short crash windows:
the durable `/config` and `/budget` grant, and the config verdict handed back to
one suspended call after restart. Together they cost approximately 800 production
lines and the larger share of configuration scenario tests. Their additional
guarantees covered a crash during an accepted command turn or between the config
write and verdict delivery. The owner accepted retyping a command after a crash
and receiving a plain rollback notice instead of a model-facing boot verdict.

The authority boundary still matters: only a manager-owned human command may
authorize a gated mutation. Configuration changes still need validation,
recoverable backups, one process-wide apply slot and rollback after failed boot.
Those guarantees do not require durable grant settlement or a suspended call.

## Decision

Keep each one-turn grant in the running session. Create it only after the
boundary commit accepts a user-source input with a non-empty manager identity
and a matching leading command. Append the host activation instruction only
when that grant is created. The command turn remains a FIFO barrier, blocks
later input while the grant is open, and permits a gated tool only as the sole
call in its response. The first matching mutation's successful execution spends
the grant, before its result commits, because the mutation's effect is already
durable. An unspent grant at ordinary run end publishes the existing keyed
unchanged notice; cancellation and restart need no durable settlement.

Make `config_edit` an ordinary tool. It parses its arguments, checks the current
grant, validates the complete document and takes the process-wide apply slot. A
guarded commit writes backup, marker and configuration in that order. Every
unsuccessful path releases the slot; a successful write holds it for the rest
of the process image. The tool returns an ordinary result and a direct receipt
telling the user that the daemon restarts. The applying session's run-end defer
signals restart once, including cancellation and panic paths. Normal completion
commits the applying session's reply before that signal.

Keep only the session ID, backup path and intended configuration hash in the
pending marker. Boot uses the existing resolution and rollback rules. Only a
rollback posts a plain, source-keyed chat notice; no boot outcome enters the
session inbox or wakes the model. The resolving boot always attempts to clear
the marker, even if notice publication fails, without retrying delivery.
Clearing compares decoded marker identity, so markers written by an older
binary clear despite obsolete extra fields, while a newer marker remains safe.

## Consequences

- A crash during an accepted command turn loses its grant. The resumed turn
  receives the ordinary authorization refusal and the user retypes the command.
- Budget mutations retain their transaction with the keyed receipt. The session
  spends authority once the mutation executes, so a failed result commit never
  posts an unchanged notice for a change that happened.
- A crash between configuration write and tool-result commit leaves a live
  configuration and an interrupted call. Boot settles it as interrupted and
  never re-executes it.
- The model receives the ordinary applied-and-restarting result before a
  normal restart. It never receives a boot-success verdict; rollback reaches
  the user alone, leaving the earlier transcript unchanged.
- Resolved markers are cleared on the first resolving boot rather than held
  for session delivery, so an undeliverable verdict cannot re-arm rollback on
  a later unrelated boot failure.
- Extra tool calls after a successful edit run in the old process image until
  the run ends. Keeping that reply brief remains prompt discipline.

## Alternatives Considered

- **Restart immediately after the step commit.** This needs a new post-commit
  tool hook and makes the next image produce the reply. Waiting for run end
  keeps the ordinary tool result and reply in the applying image.
- **Keep the marker until verdict delivery.** An undeliverable result can leave
  rollback armed for an unrelated future boot failure, the hazard identified
  in ADR-0015. Boot resolution now owns marker clearing directly.
- **Keep the grant durable and remove only verdict delivery.** This retains
  the activation table and budget replay machinery for one accepted-command
  crash window whose recovery requirement has already been dropped.
