# ADR-0030: Durable session outbox owns manager delivery

- **Status:** Accepted
- **Date:** 2026-08-24

## Context

Session input already has a durable SQLite FIFO, but several generic commands
bypass it and manager-facing output is still a best-effort session event. A
root can commit its answer while its owning manager is starting, disconnected,
or behind a full notification buffer. The transcript then contains the answer,
but the manager has no delivery intent to replay and no provider receipt that
distinguishes an unsent output from an external effect awaiting acknowledgement.

The controller is a private capability bound to one built-in manager ID. That
ownership must survive restart and clear, subagents must remain internal, and
Telegram-specific topics or message IDs must not become daemon vocabulary.
External transports cannot participate in the SQLite transaction, so strict
exactly-once provider effects are unavailable in the general case.

## Decision

We use separate durable session ledgers in both directions.

- Every person/manager action addressed to an existing session enters
  `session_inbox` before it is observed or executed. Normal messages promote to
  the transcript; generic session commands are handled from the same ledger
  without entering the LLM. Manager UI without generic session meaning—project
  spawning, pickers, service navigation, and masked secret prompts—remains with
  the manager.
- Every ordinary or lifecycle output of a manager-owned root enters
  `session_outbox`. Its creation commits with the assistant message, command
  resolution, or session lifecycle fact that made the output authoritative.
  Subagents never publish directly.
- The durable output vocabulary is replaceable message, persistent message,
  session opened, session replaced, and session closed. Heartbeat/running/idle
  activity remains best effort. The CLI-only secret protocol remains separate.
- One long-lived worker drains one global FIFO for each manager ID. Wake-ups are
  coalesced hints; startup drain and periodic rescan read SQLite. One unresolved
  head blocks every later session owned by that manager but not another manager.
- A manager invokes its transport before acknowledging SQLite. A crash after the
  external effect but before acknowledgement redelivers and may duplicate it.
  Transient failures retry indefinitely with capped backoff; permanent failures
  block the manager queue. No output is automatically skipped or coalesced.
- Acknowledgement records manager-owned external message identities. Consecutive
  replaceable outputs may reuse those identities; a persistent output may reuse
  them once and closes the chain. Managers without editing support append every
  output. Chunked renderings retain all external IDs and reconcile additions,
  edits, and deletions inside the manager.
- Sessions retain provider conversation bindings in their existing attributes.
  Outbox/inbox rows also carry JSON attributes with an immutable `manager_id`.
  A generic durable manager binding fixes manager ID to driver and non-secret
  account/destination identity before any output claim; removing and recreating
  the same ID intentionally inherits its sessions and backlog only when that
  identity still matches.
- Delivered output rows are retained indefinitely for now. The built-in CLI
  considers a successful Unix-socket write delivered; a later drop inside the
  terminal client's bounded application channel is an accepted residual risk,
  not a per-terminal durable cursor.

ADR-0021's bounded best-effort control-push rationale remains valid, but its
explicit no-replay decision is superseded. Pushes no longer carry manager output
truth; they only announce durable work or ephemeral activity.

## Consequences

- Manager startup order, dropped wakes, daemon restart, and transient outages no
  longer lose accepted session output. Disabled or removed managers retain an
  observable backlog.
- The session store gains cross-table transactions and a delivery CAS protocol;
  managers gain one shared retry worker while keeping transport rendering and
  provider identities encapsulated.
- Delivery is exactly once only at the local intent/attempt transition. External
  effects are at least once, so crash-window duplicates remain possible.
- One poison output can intentionally stop every session owned by one manager.
  Repair restores that head; there is no automatic gap or age escape hatch.
- Replaceable output is a rendering instruction, not permission to discard
  history. Non-editing transports show every version, and an offline editing
  transport may visibly replay several edits while draining.
- Manager-specific UI and masked-secret interactions retain separate protocols;
  the outbox does not become a generic UI event bus.
- The SQLite database grows with delivered output until a separate retention
  decision is made.

## Alternatives Considered

- **Gate session recovery on manager readiness.** Rejected because it covers
  only one startup window and still loses output on disconnect, overflow, or a
  process crash after recovery begins.
- **Rebuild delivery from transcript history.** Rejected because the transcript
  cannot distinguish already rendered output and does not contain status,
  waiting, lifecycle, or other out-of-band messages.
- **Use one bidirectional delivery table.** Rejected because inbox consumption
  ends in transcript/control handling, while outbox consumption ends in a remote
  effect plus provider receipt. Sharing a table obscures rather than unifies
  their state machines.
- **Acknowledge before calling the provider.** Rejected because a crash after
  acknowledgement silently loses output. At-least-once duplicates are safer
  than an unreported result that never appears.
- **Drain independently per session.** Rejected in favor of one manager-global
  order and failure boundary. A stuck output blocks that manager honestly while
  other managers remain independent.
- **Coalesce undelivered replaceable output.** Rejected because transports that
  cannot edit are expected to render every item; transport capability must not
  change which durable intents exist.
- **Require terminal-side CLI receipts.** Rejected because the local Unix socket
  is the CLI manager's transport boundary just as a successful API call is for a
  network manager. Per-terminal cursors are a separate product contract.
