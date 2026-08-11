# ADR-0010: Request max_tokens is clamped to the context-window output reserve

- **Status:** Accepted
- **Date:** 2026-08-09

## Context

Since [ADR-0003](0003-external-model-catalogs.md) the catalog is the only source of model limits, and the OpenAI-compatible client sends the catalog's max output tokens as the request `max_tokens`. Providers enforce `input + max_tokens ≤ context window`, and OpenRouter's catalog reports `max_completion_tokens == context_length` for a class of models (kimi-k2.5, glm-5 and others) — their way of saying "no separate output limit". Sending that value verbatim makes every request overflow the window by construction: a 400 loop no amount of input trimming can escape.

The degenerate value is only the visible half. An honest catalog limit (kimi-k2-thinking: 100k output on a 262k window) still overflows once input approaches the compaction threshold, because the compaction ladder reserves only `1 − compactionFraction` = 15% of the window for output. Computing `context − input` per request would fix both, but requires client-side token counting we deliberately don't do. Peer tools split the same way: OpenCode omits `max_tokens` and trusts the provider default; Roo Code clamps it to a fixed fraction of the window matched to its condense threshold; Cline shipped the verbatim-catalog-value bug (their #9592) in the same month we did.

## Decision

The OpenAI-compatible client clamps the request `max_tokens` to the output reserve: `min(catalog max, (1 − compactionFraction) × context window)`. The reserve fraction is the complement of the session compaction threshold, so the two invariants compose without any input estimation: compaction keeps input ≤ 85% of the window, the clamp keeps output ≤ 15%, and their sum can never exceed the window. A catalog that carries no output limit (`MaxTokens == 0`) still means "omit the field" — the clamp bounds a value we send, it does not invent one.

The Anthropic driver is out of scope: its catalog limits are honest, the clamp would visibly cut the primary path's output (64k → ~30k on a 200k window), and whether the Anthropic API rejects or silently truncates an over-window `max_tokens` is unverified.

## Consequences

- The 400-by-construction loop is gone for every model whose catalog output limit is large relative to its window, degenerate or honest.
- The composition is only as strong as the input estimate: `shouldCompact` triggers on a calibrated `len/4` estimate that can undercount (non-ASCII especially), so input can exceed 85% before compaction fires. The clamp shrinks the overflow surface from "every request" to "estimate undercount at saturation" — it does not provably eliminate the 400.
- Output on such models is capped at 15% of the window (~39k on 262k) even when the endpoint could produce more in a near-empty conversation. The cap is the price of not counting input tokens client-side.
- OpenRouter derives reasoning budgets from `max_tokens` (`effort_ratio × max_tokens`), so the clamp also shrinks thinking budgets on affected models. Bounded and predictable, but smaller than the catalog alone would suggest.
- The reserve fraction couples `llm` to `session`'s `compactionFraction` across a package boundary; changing the compaction threshold now silently changes the output cap, and the two constants must stay complementary.
- Follow-up: verify Anthropic API behavior for `input + max_tokens > window` and decide whether its driver needs the same clamp.

## Alternatives Considered

- **Treat the degenerate value as "no limit" and omit `max_tokens`** (OpenCode's shape): fixes the visible bug with one line, but leaves output at the provider's whim and does nothing for honest-but-large limits — the tail overflow survives untouched.
- **Clamp with a flat safety buffer** (`context − 1000`): the buffer covers no real input; any conversation past the buffer size overflows exactly as before.
- **Compute `context − estimated input` per request**: the accurate fix, but requires a client-side tokenizer per model family — machinery the session layer's calibrated estimate deliberately avoids putting on the request path.
- **Do nothing and surface the provider error** (aider's shape): acceptable for an interactive tool where a human trims the chat; coagent runs unattended, so a session that can only fail is a session lost.
