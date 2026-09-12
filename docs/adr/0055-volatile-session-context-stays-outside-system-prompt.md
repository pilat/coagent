# ADR-0055: Volatile session context stays outside the system prompt

- **Status:** Accepted
- **Date:** 2026-09-12

## Context

Provider prompt caching keys a request by its ordered tools, system prompt, and
message prefix. The daemon rebuilds a live session object on each activation,
so values derived during construction can change while the durable conversation
does not. The system prompt currently includes the current date, a timezone
value that may change across daylight-saving time, the latest curated-memory
inventory, and a snapshot of active background processes and subagents. Any one
of those changes invalidates the cached prefix of a long conversation.

These values have different lifecycles. Project instructions and curated memory
are opening context for a session. A model already observes memory mutations it
performs through their tool results. Active background work is a transient
activation fact whose later tool, cancellation, and completion observations can
supersede it. Current local time belongs to newly arriving input. Tool schemas
and model selection, by contrast, change the meaning of the provider request and
must remain honest cache boundaries.

Compaction already protects an immutable opening header containing project
instructions and the exact opening task, and separately reattaches a live
background projection inside its marked checkpoint. Those existing lifecycles
provide the required context without making the system prompt mutable.

## Decision

We keep volatile session context out of the system prompt.

- The system environment contains no concrete date or timezone. Existing input
  timestamp prefixes use local wall time with both the zone abbreviation and
  numeric UTC offset. New timestamps are appended; persisted timestamps are
  never rewritten.
- The opening AGENTS.md user-role row also carries the curated-memory inventory
  read for that opening turn. The row exists when either source is non-empty and
  retains the existing AGENTS.md marker, so compaction continues to preserve it
  together with the exact opening task. Ordinary activation and daemon restart
  reuse the persisted row. A new session or deliberate fresh-context opening
  reads both sources again.
- `memory_save` and `memory_delete` mutate durable project memory and report the
  result in the transcript, but never refresh the current opening snapshot.
- A non-empty create-time snapshot of advertised background processes and
  pending child links is appended once as a durable host-authored user-role row
  when an activation reaches its first provider call. It is not deduplicated.
  Empty snapshots and activations that make no provider call append nothing.
- Compaction retains its independent live-ledger background section in the
  marked checkpoint. Later verbatim observations remain authoritative over an
  older activation or checkpoint snapshot.
- Tool catalogs, tool ordering, model switches, and provider cache markers keep
  their existing behavior.

## Consequences

- Ordinary daemon activations reconstruct the same system prompt while the
  configured model, tools, and permanent instructions are unchanged.
- Memory changes become visible automatically to new opening turns, not by
  retroactively changing an existing session's prefix. Compaction may summarize
  the tool evidence of an in-session memory edit, which is accepted because the
  session's opening memory contract is intentionally frozen.
- Active background state adds a short transcript row on every model-running
  activation while work remains. This consumes tail tokens but preserves simple,
  explicit ordering and avoids another durable state or deduplication protocol.
- A compaction during such an activation can contain both its live checkpoint
  section and an activation snapshot. This bounded duplication is preferred to
  weakening restart or mid-activation continuity.
- The two-row immutable-header rule does not change: the combined project row is
  row 0 and the exact opening task is row 1. A memory-only project therefore
  still emits the marked row 0; a project with neither source starts at the task.
- Real tool-schema and model changes still invalidate provider cache entries.

## Alternatives Considered

- **Freeze date and timezone in the opening row.** Rejected because current time
  already belongs to appended input, and a frozen timezone abbreviation becomes
  misleading across daylight-saving transitions.
- **Keep curated memory in the system prompt and refresh it after every tool
  mutation.** Rejected because it invalidates the complete cached conversation
  immediately after a change the model already observed.
- **Give curated memory its own protected row.** Rejected because the existing
  second protected row is the exact opening task. Expanding the header would
  change a compaction invariant for no benefit when one opening project-context
  row can carry both sources.
- **Refresh curated memory only after compaction.** Rejected because it mutates
  the supposedly immutable opening context and makes compaction change permanent
  instructions in addition to replacing history.
- **Keep active background state in the system prompt.** Rejected because every
  producer transition invalidates the entire provider-cache prefix even though
  the state is an ordered, supersedable observation.
- **Deduplicate identical activation snapshots.** Rejected because it requires
  representing empty transitions or adding durable state to distinguish a later
  recurrence of the same rendered state. The short repeated tail row is the
  simpler and safer contract.
- **Remove the live background section from compaction.** Rejected because an
  activation can compact after its starting snapshot; compaction must retain a
  host-known current projection without relying on a lossy model summary.
