# ADR-0035: Compaction summarizes a bounded head and retains a verbatim tail

- **Status:** Accepted
- **Date:** 2026-08-29

## Context

[ADR-0012](0012-compaction-is-all-or-nothing.md) made compaction safe against a
failed summary replacing real history, but made room for the summary by marking
older tool-result bodies cleared before the provider call. It then replaced the
entire post-header transcript with model-authored text. A short programmatic
excerpt and reattached skills softened that loss without retaining the actual
recent conversation.

A long tool-heavy session exposed the cost of that order. The summarizer saw
placeholders where successful mutations and verification had been, while recent
low-information failures remained visible. Its checkpoint could not state what
had already succeeded, and the continuing model repeated discovery. If the
provider failed, the pre-summary clearing still survived.

[ADR-0013](0013-immutable-history-single-compaction-point.md) subsequently made
message bodies immutable between compactions and established one safe execution
point with no pending call. Those invariants remain necessary, but its remaining
clearing phase and ADR-0012's summary-of-everything shape no longer serve
continuity.

Coagent is multi-provider and permits model switches. A provider-native opaque
checkpoint cannot be the durable transcript for every backend. Conversely, the
runtime cannot determine which arbitrary command, MCP result or task fact is
semantically important enough to pin. It can guarantee protocol integrity and
complete evidence at the summarizer boundary; semantic coverage of the produced
summary remains model quality.

## Decision

We replace both earlier compaction decisions with one provider-neutral
checkpoint protocol.

- Automatic pressure and the durable user `/compact [focus]` command are the
  only triggers. The model-callable `compact_context` tool is removed.
- Compaction still runs only at `applyContextEvents`, when no ordinary or
  external call is pending. Stored message bodies remain append-only.
- The system prompt and current immutable transcript header remain unchanged.
  Header membership keeps the existing rule: an opening system/AGENTS row plus
  task when present, otherwise the first task row.
- One compaction attempt makes one no-tools model call with one canonical text
  input and expects one completed non-empty text output. Provider-native message,
  reasoning and image objects are not replayed.
- The canonical input contains the existing repaired model projection of only
  the older head. It records roles, text, complete tool-call/result ownership,
  raw argument bytes and attachment metadata. The retained tail is never sent to
  the summarizer.
- The complete summarizer request may estimate at no more than 50% of the model's
  catalog context window. At least 10% of the window remains as a repair-free
  verbatim raw suffix when that much history exists. The tail has no maximum;
  every additional message not admitted to the bounded head stays in it.
  Automatic and manual compaction use the same split.
- The summary is wrapped in a host-authored marker that says it describes older,
  lossy context and that later verbatim messages take precedence. No fabricated
  assistant acknowledgement or separate continuation primer is inserted.
- The latest successful skill invocation is the current skill. If its exact
  envelope remains in the tail, it is not duplicated. If it enters the head, its
  activation is excluded from the history-to-summarize and the envelope is
  inserted verbatim between the marked summary and tail. A skill reattached by a
  previous checkpoint is scaffolding: every later compaction excludes it from
  model summarization and carries the same envelope forward unchanged. Earlier
  skills remain historical input and are summarized normally. The current skill
  is never truncated, summarized or accumulated into duplicate copies.
- Host-known active background work remains a separate section inside the marked
  checkpoint and is not part of the next model-summary anchor. Permanent
  instructions and durable TODO state retain their existing independent
  lifecycles.
- On repeated compaction, the previous model summary is the anchor and only raw
  messages newly leaving the tail form the delta. A raw group is incorporated
  into at most one successful checkpoint; failed attempts may submit it again.
- The provider receives the normal full output reserve. Missing headings or a
  short answer are not failures. Empty, tool-calling, cancelled, unknown-finish
  or length-stopped output is not a checkpoint.
- Before committing, the runtime estimates the exact next ordinary projection.
  A candidate above the existing 85% trigger is non-relieving and rejected;
  equality is relieving because the trigger is strict greater-than.
- Success atomically marks only head rows `compacted_at`, appends the summary,
  and positions the existing header, summary, optional current skill and existing
  tail. Rows committed outside the snapshot remain a newer NULL-position suffix.
  Failure changes no active transcript metadata.
- `cleared_at` and the duplicate `sessions.compaction_brief` copy are removed.
  The active marked summary message is the incremental anchor after restart.

The runtime validates and tests serialization, pairing, boundary selection,
atomicity, ordering and restart. It does not claim to prove that every model
will choose every important semantic fact. Rejected compaction response usage
continues to be absent from message-derived lifetime totals under the existing
provider-usage-ledger technical debt.

## Consequences

- A successful checkpoint retains an original recent suffix rather than a quoted
  excerpt, and its summarizer sees old tool evidence instead of uniform cleared
  placeholders.
- A failed, empty, truncated or non-relieving attempt leaves the active context
  exactly as it was. Old result bodies no longer disappear before a provider
  call.
- Compaction is intentionally less aggressive. Near the automatic 85% trigger,
  a 50%-bounded summarizer commonly leaves roughly 35% or more as raw tail. That
  costs more cached input than a 10% hard tail but leaves ample room and avoids a
  multi-call chunking protocol.
- The same canonical checkpoint works after a model or provider switch. Opaque
  reasoning payloads and old image pixels are not part of semantic summarization;
  native refs still survive when their message is retained in the tail.
- Skill continuity becomes a first-class invariant. Any future checkpoint change
  must prove the current envelope survives byte-for-byte exactly once across
  head/tail movement, failures, restart and repeated compaction.
- Dropping `cleared_at` reveals the stored body of any historically active
  cleared result. Already compacted rows remain hidden by `compacted_at`. A
  revealed row carries no position, so on an upgraded mid-flight session it
  loads after the positioned tail as a newer raw suffix — provider-valid
  through repair, and the next pressure crossing handles it like any appended
  work.
- Semantic omissions remain possible with a weak summarizer. Representative
  evaluations measure that model-quality property; protocol tests cannot turn it
  into a runtime guarantee.
- Failed completed compaction calls can still undercount cost until the complete
  provider-usage ledger recorded in `ai/techdebts.md` is implemented.

## Alternatives Considered

- **Keep trim-before-summary and summarize the whole active conversation.**
  Rejected because it removes the evidence the summarizer needs and makes a
  failed attempt mutate the model projection.
- **Use provider-native compaction.** Rejected because opaque checkpoint items
  are not portable, inspectable or uniformly replayable across coagent's
  configured backends.
- **Replay native tool messages to an ordinary summarizer.** Rejected because
  provider contracts for tools, reasoning signatures and images differ. One
  canonical text is the stable cross-provider boundary.
- **Chunk a large head through several summary calls.** Rejected because it adds
  cost, partial-failure states and summary-of-summary degradation. A 50%-bounded
  head plus unbounded raw remainder achieves one-call pressure relief.
- **Use a hard 10% tail maximum.** Rejected because a complete newest protocol
  group can exceed it. The minimum-tail/maximum-head formulation never splits a
  call group and leaves the remainder verbatim.
- **Validate required Markdown sections or ask a second model to judge the
  summary.** Rejected because structure is not semantic completeness and another
  model does not turn arbitrary tool importance into a runtime fact.
- **Pin selected mutations, tests or identifiers in a deterministic evidence
  ledger.** Rejected for this initiative because generic tools expose no common
  importance contract. Known permanent instructions, TODO and active background
  state already have independent owners.
- **Reattach every historical skill.** Rejected because obsolete workflow phases
  consume context and can conflict. Only the latest successful skill remains
  current.
