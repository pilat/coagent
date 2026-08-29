# ADR-0013: History is immutable between compactions; compaction is the only automatic pressure response

- **Status:** Superseded by [ADR-0035](0035-compaction-summarizes-a-bounded-head.md)
- **Date:** 2026-08-16

## Context

Context pressure used to be managed by a ladder: an automatic tool-result
clearing stage each iteration, a model-invoked `clear_tool_results` tool, and
compaction as the last rung. The follow-up design (after ADR-0012) planned to
go further — continuous pruning every iteration, gated by prompt-cache cooldown,
like opencode's `prune` and openclaw's TTL-gated trimming.

Two facts killed that direction. First, retroactive mutation of old messages
invalidates the provider's prompt-cache prefix from the edited point onward;
client-side agents that tried it are moving away (hermes-agent rejected
opencode-style pruning as "would destroy prefix cache hit rates" and trims at
insertion time instead), and Anthropic's own continuous clearing
(`clear_tool_uses`) is server-side *after* cache lookup — an option a
multi-provider client does not have. Second, the one continuous mechanism that
is cache-free already exists here: tool output is truncated at execution time
(`toolexec.go`), before it ever enters the transcript.

Separately, compaction had three entry points — the per-iteration ladder, the
`compact_context` tool, and `/compact` calling `compact()` directly from the
boundary handler. The direct slash path ran with a pending external call still
unanswered in the transcript, and compacting away that `tool_use` made the
subagent's eventual result undeliverable forever (a zombie link). The planned
fix was "pinning": surgically excluding pending-call messages from the
summarized range.

## Decision

- **Between compactions, transcript content is never mutated.** No continuous
  pruning, no automatic clearing stage, and the `clear_tool_results` tool is
  removed entirely. No decision anywhere keys on prompt-cache state (TTL,
  cooldown). Insertion-time truncation is the only continuous relief; clearing
  survives only *inside* `compact()` as feed preparation for the summarizer.
- **The trigger is the provider's own count.** `shouldCompact` projects the
  last real response's cache-inclusive `PromptTokens` plus a `len/4` delta of
  messages appended since; the calibration machinery (scale factor, overhead)
  is deleted. No measurement (fresh session, resume, subagent, right after
  compaction, model switch) falls back to the plain estimate.
- **Compaction has exactly one sanctioned execution point** —
  `applyContextEvents` in the agent loop, which is also the one place that
  decides compaction is safe: it returns early while any tool call is pending,
  leaving a queued request queued rather than consuming it into a failure.
  `/compact` and `compact_context` set a flag consumed there; nothing calls
  `compact()` directly. A `/compact` arriving while a non-sleep external call is
  pending stays in the durable inbox and executes at resume (sleep is
  interrupted, like any user input). Inside `compact()` the same check remains
  as defense in depth, not as a working mechanism.

## Consequences

- The prompt prefix stays byte-stable between compactions: cache reads at the
  discounted rate for the whole active phase, invalidated only when compaction
  rebuilds the transcript anyway.
- Pinning surgery is unnecessary: the only path that could meet a hanging
  `tool_use` was the direct slash entry, and it no longer exists. The zombie
  subagent-link failure mode is closed by construction, not by special-casing.
- Context pressure relief is coarser: nothing shrinks a hot conversation until
  the threshold fires. The threshold's 15% headroom and execution-time
  truncation carry that load.
- The automatic path can give up. Three consecutive compactions that leave the
  projection above the threshold silence it for the rest of the activation, with
  one notice that the window is too small for the workload. Explicit requests
  are never capped, and a new activation starts from a clean counter — that is
  the only "conditions changed" signal available.
- A deferred `/compact` costs nothing functionally: a sleeping session makes
  no requests, so compacting at wake yields the model the same context as
  compacting at request time.

## Alternatives Considered

- **Continuous pruning gated by cache cooldown** (openclaw pattern). Rejected:
  the gate never fires during active work (cache always warm), so it defers
  all relief to idle boundaries anyway, while adding cache-state coupling the
  user explicitly banned.
- **Unconditional continuous pruning** (opencode pattern). Rejected: breaks
  the cached prefix repeatedly during the hottest phase; the field is moving
  to insertion-time trimming for exactly this reason.
- **Pinning pending calls through compaction.** Rejected: complexity serving a
  single reachable path, which the flag-based entry removes outright.
- **Refusing `/compact` while suspended.** Rejected in favor of durable
  queueing: the inbox already provides exactly the "behind the pending call"
  semantics, survives daemon restarts, and needs no new state.
