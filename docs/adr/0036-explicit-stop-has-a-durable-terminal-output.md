# ADR-0036: Explicit stop has a durable terminal output

- **Status:** Accepted
- **Date:** 2026-08-29

## Context

ADR-0025 makes `/stop` a durable two-phase park: fence the tree, cancel and join its runners, settle unresolved calls, clean producer ledgers, then mark sessions stopped. The user-visible protocol did not complete the same transition. It committed `⏹ Stopping...` immediately as a persistent releasing output, then represented successful cleanup only as an ephemeral idle state event. Telegram renders durable outbox messages, not that state event, so the conversation could remain on “Stopping” forever, including after startup finished an interrupted stop.

Explicit `/compact` already demonstrates the intended interaction: a replaceable started message becomes a persistent terminal result. Stop needs the same visible shape, but its terminal claim must not precede call settlement, descendant parking, or armed-budget release.

## Decision

An explicit manager-owned `/stop` writes a replaceable, non-releasing `⏳ Stopping…` output in the same transaction as its durable stopping fence. The start identity is `input:<id>:stop:started`.

After runners join and every descendant obligation is settled, one session-store transaction releases an armed budget with reason `stopped`, marks the explicit root stopped, and inserts persistent releasing output `⏸️ Session stopped` under `input:<id>:stop:completed`. Failure rolls the whole terminal transaction back, leaving the root stopping and publishing no success. Successful completion sends a non-authoritative delivery wake; the outbox remains the source of truth.

Startup selects the newest handled stop input with a started output and no completed output, finishes the same idempotent cleanup, and commits the same terminal transaction. It also recognizes the prior `input:<id>:stop:result` start identity during upgrade recovery. Historical completed stops never qualify. Startup completes stopping only; it never invokes the model or re-executes stopped tool work.

Readiness is tied to the newest releasing outbox obligation. Acknowledging an older final output after stop has committed cannot announce readiness while the stop completion row remains undelivered.

Budget parking, ownerless/direct stop, child-internal stop, and `/stop` addressed to an already-stopped root retain their separate output paths. A later ordinary input is a new model turn on the settled transcript, not resumption of a stopped call.

## Consequences

- Telegram normally edits one “Stopping” card into “Session stopped”; an intervening persistent outbox row may legitimately make the terminal result a new message.
- Readiness is released only after both durable stop completion and terminal-output delivery.
- Explicit stop recovery now has a durable user-visible terminal obligation in addition to transcript/status cleanup.
- Root finalization must remain separate from descendant parking until the terminal transaction; a sequential status update followed by output insertion is forbidden.
- Budget mutation and direct-output paths must fail closed after the stopping fence so new receipts cannot be created by work that stop already excluded.
- Stop tests must cover failures and crashes at the start fence, cleanup boundaries, terminal transaction, delivery wake, and later fresh input.

## Alternatives Considered

- **Keep only the persistent “Stopping” acknowledgement.** Rejected because it describes an intermediate state as a permanent result and never confirms success.
- **Rely on the final idle state event.** Rejected because manager output is durable outbox state; Telegram treats observer events only as wake hints.
- **Append success after marking the root stopped.** Rejected because a crash between those writes recreates the stuck intermediate message with no recoverable terminal obligation.
- **Commit success before armed-budget release.** Rejected because a later release failure would contradict the already-published success.
- **Emit explicit stop output from generic tree cleanup.** Rejected because budget parking and internal/ownerless callers use the same cleanup without owning a `/stop` user interaction.
