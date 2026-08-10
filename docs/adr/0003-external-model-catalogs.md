# ADR-0003: External model catalogs replace config-carried model metadata

- **Status:** Accepted
- **Date:** 2026-08-07

## Context

Every model in `config.yaml` carries `name`, `context_window`, and `max_tokens` by hand, backed by two hardcoded fallback tables in `internal/llm` (`knownContextWindows`, `hardcodedPrices`) that drift the moment a provider ships a new model. Reasoning-effort support needs per-model capability knowledge (does this model reason at all, what budget bounds apply) that config never carried. Anthropic's own `/v1/models` API returns ids and display names but no limits, so "ask the provider" is not universally possible — while OpenRouter's `/api/v1/models` returns everything including reasoning support, and public community catalogs (models.dev, LiteLLM) maintain limits, pricing, and reasoning metadata for every major provider.

The tension: first-party APIs are authoritative but incomplete across providers; community catalogs are complete but third-party; config-carried metadata is offline-safe but permanently stale.

## Decision

Every provider driver implements the private `driverProtocol` interface whose contract includes `ListModels`: fetch the provider's available models and their characteristics (display name, context window, max output tokens, pricing, reasoning support) from an external source. Where the data comes from is each driver's own implementation — shared fetch/cache helpers may be reused, but the entry point is always the driver's method, and adding a driver means implementing it. Sources are native per driver: the `openrouter` driver uses OpenRouter's first-party `/api/v1/models`; the `anthropic`, `openai`, and `google-sa` drivers use models.dev (`models.dev/api.json`), selecting the provider section via a per-provider config key where the driver name alone is ambiguous.

Catalogs are fetched once at daemon startup with a short timeout and cached on disk (`~/.coagent/cache/`); an offline start falls back to the last cached snapshot. Refresh is by daemon restart — no background TTL.

Config model entries shrink to `[id, provider]` and **the catalog is the only source of truth**: the old `name`/`context_window`/`max_tokens`/`pricing` fields are removed from the config schema entirely — no override mechanism exists. A configured model whose metadata cannot be resolved from its driver's catalog is a **fatal error at startup**. Pricing likewise comes from the catalog alone, retiring every hardcoded table including the default-price fallback (unknown cost is reported as zero, not guessed). Reasoning-effort budgets for drivers that need `budget_tokens` are derived from catalog data (fraction of the model's output limit, per OpenRouter's documented effort convention, clamped to the catalog's minimum) — never from hardcoded per-model tables.

## Consequences

- Config shrinks to intent (`id`, `provider`); metadata stops drifting and new models need no config archaeology.
- The picker can render `Provider/Display Name` plus live pricing; effort availability is model-dependent for free (catalog says whether the model reasons).
- `knownContextWindows`, `hardcodedPrices`, and `defaultPrice` are deleted; exactly one resolution path remains.
- First daemon start needs network — a machine that has never fetched a catalog and is offline fails startup. Accepted: the daemon's job requires network anyway.
- A model removed from its catalog turns a previously working config into a startup failure once the cache refreshes. Accepted — the config must name models that exist.
- A model absent from every reachable catalog (a truly uncatalogued local model) cannot be configured at all. Accepted deliberately: an override mechanism would reintroduce a second source of truth and the drift that comes with it.
- models.dev becomes a load-bearing third-party dependency for non-OpenRouter drivers, mitigated by the disk cache. Dated-vs-undated model id mismatches must be handled in the driver's id matching.

## Alternatives Considered

- **Keep config-carried metadata (status quo).** Rejected: every new model requires the user to research and transcribe limits; the hardcoded fallbacks drift; per-model reasoning capability would need yet another hand-maintained field.
- **Config fields as optional overrides on top of the catalog.** Rejected: two sources of truth for the same fields recreate the drift problem in a subtler form ("why does this model report 128k when the catalog says 200k"). The catalog is authoritative or it is nothing.
- **A standalone catalog service dispatching on driver strings.** Rejected in favor of a method on the driver interface: catalog knowledge belongs to the driver that owns the provider protocol, and a new driver must be forced by the compiler to answer where its models come from.
- **Single universal models.dev source for all drivers.** Rejected: for OpenRouter it is second-hand data when a first-party API exists with routing-specific fields (`supported_parameters`, per-endpoint max tokens).
- **LiteLLM `model_prices_and_context_window.json` as primary.** A flat ~3000-entry map keyed by ad-hoc names, costs per token (not per 1M), no explicit budget-token bounds. Broader coverage and very actively maintained — kept as the documented fallback candidate if models.dev degrades.
- **Background TTL refresh.** Rejected: model catalogs change slower than the daemon restarts; a refresh loop is a moving part with no payoff.
