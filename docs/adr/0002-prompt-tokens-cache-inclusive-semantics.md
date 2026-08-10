# ADR-0002: `PromptTokens` counts total input including cache, uniformly across providers

- **Status:** Accepted
- **Date:** 2026-08-04

## Context

Every LLM call records token usage into `llmwire.MessageUsage`
(`PromptTokens`, `CompletionTokens`, `CacheTokens`, `CacheWriteTokens`), consumed for three
things: the context-window occupancy shown in `/status`, session cost, and the compaction
trigger (`shouldCompact`).

Providers disagree on what "prompt tokens" means once prompt caching is active:

- **Anthropic Messages API:** `input_tokens` is **only the uncached remainder** — the tokens
  after the last cache breakpoint. True input = `input_tokens + cache_read_input_tokens +
  cache_creation_input_tokens` (per Claude docs).
- **OpenAI / OpenRouter / Gemini via OpenAI-compat:** `prompt_tokens` is the **full input**;
  `cached_tokens` / `cache_write_tokens` are subsets *inside* it. Verified empirically for
  OpenRouter→Claude (bedrock-routed): on a cache-read turn `prompt_tokens=9512`,
  `cached_tokens=9504` (a subset, remainder 8 uncached).

The native Anthropic extractor set `PromptTokens = input_tokens` — the uncached remainder.
Because coagent marks ephemeral cache breakpoints, in steady state that is a tiny delta, so on
Anthropic two things broke silently: `/status` "context used" understated the real window by the
entire cached prefix, and `estimateCost`'s `effectiveInput = PromptTokens - Cache - CacheWrite`
went negative and clamped to `0`, billing fresh input at zero. The same field meant different
things on different providers, and nothing at the type level said which.

## Decision

We define `MessageUsage.PromptTokens` to mean **the total input tokens the model processed for
the call, cache included** — the same meaning for every provider — and we document that contract
on the `llm.Client` interface / `MessageUsage` type so a new implementer inherits a written rule
rather than a convention to reverse-engineer.

- OpenAI-compatible providers already satisfy it; no change.
- The Anthropic extractor is corrected to
  `PromptTokens = input_tokens + cache_read_input_tokens + cache_creation_input_tokens`.
- `CacheTokens` (cache read) and `CacheWriteTokens` (cache write) remain sub-breakdowns and are
  always **subsets** of `PromptTokens`. The load-bearing invariant is
  `PromptTokens >= CacheTokens + CacheWriteTokens`; `estimateCost` already depends on it.
- A defensive guard on the OpenAI-compat path treats a violated invariant
  (`cached + write > prompt_tokens`) as a provider that *excludes* cache and adds the missing
  tokens back. It is a canary for a future/changed provider — empirically dormant today.

## Consequences

- Context-window occupancy (the `/status` bar) and the compaction trigger read a real,
  cache-inclusive number on every provider. The Anthropic understatement and the zero-rate
  fresh-input billing bug are fixed by the same one-function change.
- The field's meaning is uniform and documented on the interface. Enforcement stays convention +
  per-provider tests + the runtime canary, **not** the type system: providers return incompatible
  raw shapes (OpenAI total-with-cached-subset vs Anthropic uncached-plus-split), so there is no
  single "raw" field a shared helper could normalize for free — per-provider knowledge must live
  somewhere.
- Historical `messages` rows written before the change keep the old (Anthropic-uncached) value.
  Occupancy and the trigger read the latest turn (self-heals within one turn); lifetime accounting
  sums per-call cost, so mixed-meaning rows do not corrupt money — only a resumed old session's
  first `/status` occupancy may momentarily understate.
- Per-provider table tests pin the mapping (Anthropic sums three; OpenAI/OpenRouter pass through)
  and assert the invariant.

## Alternatives Considered

- **Add a new `ContextTokens` field, leave `PromptTokens` as each provider's raw value.**
  Rejected: leaves `PromptTokens` provider-inconsistent (Anthropic uncached vs OpenAI total) — a
  field that still lies by provider — and putting a correct field beside a wrong one does not
  remove the need to fix the wrong one. Once `PromptTokens` means total, the extra field is
  redundant.
- **Redefine `PromptTokens` as uncached/billable-new everywhere (+ a separate total field).**
  Rejected: also requires changing the OpenAI path (`prompt_tokens - cached`), churns more code,
  and makes historical OpenAI rows inconsistent too — for no gain over fixing Anthropic to match
  the OpenAI convention `estimateCost` already assumes.
- **Structurally enforce the contract via shared normalization instead of per-provider mapping.**
  Rejected as not free: incompatible provider shapes mean per-provider knowledge is unavoidable;
  the invariant test plus runtime canary are the pragmatic enforcement.
