# ADR-0012: Compaction trims, then summarizes the whole conversation — or fails

- **Status:** Accepted
- **Date:** 2026-08-15

## Context

Compaction used to be a cascade of degradations. It summarized only the slice it
was about to replace — not the opening task, not the retained tail — after
splitting that slice into 8000-token chunks, one LLM call each, every partial
held to the same three-section contract as a finished brief. A fragment cannot
state a Goal, so each chunk burned its full three-attempt quality budget and the
failing partial was accepted anyway. When a call failed for any reason the code
walked down stages: retry without oversized messages, then synthesize
`"Previous context was lost during compaction"` and write THAT over the
transcript it was meant to summarize.

A real `/compact` with instructions (session 114) hit every one of these: three
doomed calls on a three-message fragment, whose model wrote back "the
conversation provided is very thin"; then a stopped daemon cancelled the next
call, which the code read as "summarization failed" and answered with the
placeholder. 179 messages survived only because the durable write failed under
the same cancelled context. A provider 5xx with a live context would have
committed it.

The tension is that a summarizer needs the conversation, and the conversation is
by definition near the context window when compaction fires — 85% of it, the
`compactionFraction` trigger. Feeding it back in whole looks impossible, which is
what the chunking and the slice-only input were working around.

## Decision

Compaction is one operation with one outcome: a real summary of the whole
conversation, or an error that leaves the conversation alone.

- **Trim, then summarize.** `compact()` first applies the tool-result clear at
  the same `keepRecent` as its own summarization boundary, so the bodies it is
  about to replace go in as placeholders. Tool output is the bulk of a
  transcript and a summary never needed it verbatim; removing it is what makes
  the whole conversation affordable as one prompt.
- **Everything else goes in whole.** One LLM call over the entire remaining
  conversation — opening task, latest rounds, and all. No chunking, no
  map-reduce, no per-message head/tail cut (tool output is already truncated at
  execution time by `toolexec.go`). The incremental path omits only the previous
  summary/ack/primer/reattachment rows, whose brief is passed in directly. No
  verbatim tail survives the rebuild; `keepRecent` bounds what the summarizer
  sees uncleared, and a programmatic capped excerpt of the last turns rides in
  the summary message.
- **The section contract binds the final brief only**, which is now the only
  output there is.
- **No degradation.** An over-budget payload, a cancelled run, a transport or
  provider failure all return an error. There is no placeholder brief, no
  partial summary, no "N messages were not incorporated" note. The caller
  reports the failure and the conversation stays as it was.
- **The budget is the window minus writing room** (`compactionOutputReserve`),
  NOT the `compactionFraction` trigger. The payload *is* the conversation, which
  is above that fraction whenever the automatic path fires — budgeting by the
  same number refuses every compaction it asks for. A test pinned this: 180k
  payload against a 170k budget on a 200k window.
- **The same reserve caps the request's own output.** The summarization call
  carries `WithMaxTokens(compactionOutputReserve)`, so neither the first brief
  nor any later merge can outgrow the room reserved for it. The Anthropic
  thinking budget is sized against that effective cap rather than the client's
  own limit — otherwise every compaction on a reasoning model is a deterministic
  400. This is per-call and deliberate: the clamp in
  [ADR-0010](0010-output-budget-clamps-max-tokens.md) explicitly excludes
  Anthropic, which is the main path.
- **Token accounting is anchored on the provider.** The trigger projects the
  last real response's cache-inclusive `PromptTokens` plus a `len/4` delta of
  messages appended since that measurement; with no measurement it falls back
  to the plain estimate. The earlier scale-factor calibration was superseded by
  [ADR-0013](0013-immutable-history-single-compaction-point.md); the
  summarization budget check uses the plain `len/4` estimate.

## Consequences

- A failed compaction can no longer destroy work. The worst outcome is "trimmed
  but not summarized" instead of "transcript replaced by a note saying the
  transcript was lost".
- **The trim survives a failed summarization, deliberately.** A cleared result
  loses nothing recoverable — the tool call stays visible and re-running returns
  the data — and the trim already freed the context the operation was called to
  free. Undoing it would mean either restoring bodies the store
  has already marked cleared, or refusing to trim until after the summary, which
  puts the untrimmed conversation back in the prompt and defeats the decision.
- A brief now knows where the work started and where it stands, so a
  `/compact <focus>` like "не забудь с чего всё началось и что имеется сейчас"
  is answerable at all.
- Cost per compaction drops from up to `3 × chunks` calls to one.
- A conversation whose header (project context plus system prompt) alone
  exceeds the threshold cannot be compacted — it errors every time with an
  explicit "model too small" message, and the session keeps running against a
  full window until the user intervenes. This is the honest failure of a window
  too small for the work, not something to paper over.
- The projection is still an estimate between measurements: exact for the
  request it was measured on, `len/4`-approximate for the appended tail. It
  sharpens the input half of
  [ADR-0010](0010-output-budget-clamps-max-tokens.md)'s composition without
  making it provable.

## Alternatives Considered

- **Keep chunking as an emergency path for tiny windows.** Written and then
  removed: it kept the failure mode alive in the least-tested corner, and a
  window too small to hold its own trimmed conversation cannot summarize it
  usefully anyway. Failing loudly is better than a merged pile of fragment
  summaries.
- **Keep the generic placeholder as a last resort.** It is indistinguishable
  from a successful compaction to every later reader, including the model. A
  placeholder that replaces real content is data loss with a friendly message.
- **Truncate each message head/tail before summarizing** (the previous 16000-char
  cap). Redundant: tool output is already capped at execution time, and cutting
  the middle out of user and assistant turns loses exactly the reasoning a brief
  is supposed to carry. OpenCode makes the same split — truncation at tool
  execution, nothing extra at compaction.
- **Budget by `compactionFraction`, like the trigger.** Refuses every
  auto-compaction by construction, since compaction is only called when the
  conversation is above that fraction.
- **Count tokens with a real tokenizer.** Correct and expensive per provider;
  the provider's own reported usage is free, already in the response, and is
  what the billing is based on.
