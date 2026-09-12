# ADR-0056: Compaction replays the native bounded prefix

- **Status:** Accepted
- **Date:** 2026-09-12

## Context

[ADR-0035](0035-compaction-summarizes-a-bounded-head.md) retained a recent raw
tail but converted the older head into one provider-neutral JSONL user message.
Later attempts sent the previous summary plus only the newly aged delta. This
kept the durable checkpoint portable, but the summarizer request no longer
shared the ordinary conversation's message prefix and could not benefit from
provider caching of that prefix.

The recent tail still must remain outside compaction. It contains the freshest
model response and completed tool work, preserves continuity verbatim, and gives
the next ordinary turn unsummarized evidence. The change is therefore about the
representation of the bounded head, not moving the split to the latest request
boundary or the end of the transcript.

## Decision

We send the selected older head to the summarizer as the native conversation
prefix.

- The existing selector still chooses one legal split between an older head and
  a non-empty recent raw tail. The tail targets at least 10% of the context
  window when enough history exists, respects tool groups and image ceilings,
  is not sent to the summarizer, and is carried through commit byte-for-byte.
- The summarizer receives the ordinary system prompt, ordered active tool
  schemas, tool-choice behavior, reasoning configuration, and ordinary repaired
  messages from the current transcript beginning through the selected split.
  It then receives one final user instruction to produce the checkpoint.
- Repeated compaction again starts at the current transcript beginning. The
  native prefix therefore includes the prior marked summary, a reattached
  current skill, and any former-tail rows that have since aged into the head.
  There is no separate summary anchor or delta-only request.
- Split selection first performs the complete legal search under the existing
  50% request target. If no legal non-empty-head/non-empty-tail candidate fits,
  it repeats the search under the ordinary 85% input ceiling. The fallback
  applies to all mandatory request input; 50% is not a failure boundary. The
  15% output reserve and post-compaction relief check remain.
- Compaction does not add cache markers or persist a cache boundary. Provider
  cache admission remains outside the checkpoint protocol.
- The final instruction tells the model not to use tools. A returned client tool
  call is answered once in role — tool results reporting that tools are
  unavailable and the demand to summarize restated — and the request retries
  once with the nudge inside the same summarizer call; its cost joins the
  summary row. A second tool-calling answer is rejected without execution.
  Provider-executed tools enabled by the ordinary configuration are not
  special-cased because changing tool choice would change the request shape
  being preserved.
- Compaction uses the ordinary transcript repair without its former additional
  malformed-call sanitizer. Provider rejection of such history fails atomically
  instead of rewriting the native prefix.
- The durable output format is unchanged: immutable header, new marked summary,
  optional byte-identical current-skill envelope, then the exact retained tail.
  Failure atomicity, active-background ownership, concurrency ordering, restart
  behavior, and usage accounting remain unchanged.

## Consequences

The compaction request can share its long initial message sequence with ordinary
requests, allowing providers to reuse that prefix when their cache admits it.
Exact cache hits and savings remain provider policy, not a runtime guarantee.
Providers with an existing explicit prompt-marker policy keep it; improving an
admitted partial prefix is a separate decision.

Native replay restores real message roles, calls, results, images, and sealed
reasoning to summarizer input and delegates their wire conversion to the active
driver. A model switch therefore uses the same conversion as the next ordinary
request. The canonical JSONL serializer and incremental-input machinery can be
removed.

The 50% request target is now soft in the exceptional case where no legal
candidate fits it. A fallback request can consume up to 85% of the window but
still retains a non-empty raw tail and reserves the ordinary output fraction.
Exposing ordinary tools means a provider may act on its own server-side tool
configuration despite the instruction; preserving request identity is the
chosen tradeoff. Malformed native history can likewise cause an atomic provider
failure rather than a cache-breaking compaction rewrite.

## Alternatives Considered

- **Keep canonical JSONL and incremental summary-plus-delta input.** This retains
  provider neutrality inside the prompt but structurally differs from the
  already-sent conversation, defeating the cache-preservation goal.
- **Move the split to the last ordinary request boundary.** This would maximize
  one historical prefix match by sacrificing the bounded recent tail. The tail
  is a continuity invariant and must remain unchanged.
- **Disable tool choice for compaction.** `none` prevents tool use but changes the
  request shape. We keep the ordinary schemas and choice and reject client tool
  calls instead.
- **Persist cache boundaries or add proactive breakpoints.** Cache admission is
  provider policy and does not belong in durable conversation state. Native
  replay achieves the required structural match without a second protocol.
- **Keep 50% as a hard failure boundary.** Mandatory prefix content can make the
  target impossible even when a useful bounded request fits the ordinary 85%
  input ceiling.
- **Sanitize malformed calls only during compaction.** That can make a request
  provider-valid but stops it matching the ordinary projected prefix at the
  first rewritten row.
- **Use provider-native checkpoint objects or multi-call chunking.** Both add
  provider-specific durable state or a new partial-progress protocol without
  preserving the requested single bounded-prefix replacement.
