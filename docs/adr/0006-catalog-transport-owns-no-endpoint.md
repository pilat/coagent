# ADR-0006: The catalog package is transport only — endpoints and formats belong to drivers

- **Status:** Accepted
- **Date:** 2026-08-08

## Context

[ADR-0003](0003-external-model-catalogs.md) made model metadata a **driver** responsibility: every implementation of the private `llm.driverProtocol` implements `ListModels`, and "where my models' characteristics come from is my business". Shared fetch/cache helpers were explicitly permitted so the two drivers reading the same community catalog would not each reimplement HTTP and disk caching.

The implementation drifted past "helper". `internal/catalog` ended up exposing a `Fetcher` interface whose methods were *named after specific catalogs* — `ModelsDev(ctx)` and `OpenRouter(ctx, url)` — alongside `ModelsDevURL` and `DefaultOpenRouterURL` constants, an `OpenRouterURL(baseURL)` derivation function, and both wire-format parsers. A driver did not supply its catalog; it picked one of two the shared package already knew about.

That inverts ADR-0003. Adding a driver backed by a third source (the documented LiteLLM fallback, a self-hosted metadata service) meant editing the shared package: a new interface method, a new constant, a new parser — and every existing driver recompiled against a widened interface it does not use. The driver contract guaranteed a driver *declares* where its models come from, while the type it depended on guaranteed it could only choose from a hardcoded menu.

## Decision

`internal/catalog` is transport and vocabulary. It names no endpoint and parses no payload format.

The driver passes a `Source` describing its catalog completely: `URL`, `CacheName`, and `Validate func([]byte) error`. `Fetcher` collapses to one method, `Fetch(ctx, Source) ([]byte, error)`, returning a raw body. Endpoint constants (`modelsDevURL`, `defaultOpenRouterURL`), the base-URL derivation for self-hosted OpenRouter gateways, and both wire-format parsers move into `internal/llm` next to the drivers that own them — `catalog_modelsdev.go` (shared by the `anthropic`, `openai` and `google-sa` drivers) and `catalog_openrouter.go`.

`Source.Validate` is the driver's own parser handed to the transport, which preserves the parse-before-write guarantee without the transport knowing any format: a body the driver rejects is neither cached nor returned, so a 200 carrying an error page cannot clobber a good snapshot and cannot reach enrichment.

Per-process memoization generalizes from a `sync.Once` over models.dev to a per-URL memo, keeping the property that matters — three drivers reading one catalog cause one request — without that catalog being special.

What stays in `catalog` is what is genuinely common: `ModelSpec` (the shape every parser produces), `Lookup`/`Flatten` (id matching and section merging), and the effort vocabulary (`SortEfforts`, `ClampEffort`, and their private rank helper, per [ADR-0005](0005-per-model-effort-allowlist.md)).

## Consequences

- Adding a driver with a new catalog touches only that driver: a `Source` and a parser next to it. The shared package is not edited, and no existing driver recompiles against a widened interface.
- `Fetcher` is one method, so a test fake is a few lines and cannot drift out of sync with an interface that grows per catalog.
- The transport's own tests can no longer reference a real catalog — they invent a payload and a validator. That is the intended constraint: a test that needs models.dev proves the boundary leaked.
- `internal/llm` grew two files. Accepted: format knowledge sitting beside the driver that needs it is the point, and the models.dev parser is shared by three drivers *inside the same package*, so nothing is duplicated.
- One risk is worth naming: a driver may now pass a `Source` with no `Validate`, and the transport will cache whatever arrives. The transport cannot detect this, since it has no idea what a valid body looks like. The guard is a package anti-pattern note, not a type.

## Alternatives Considered

**Keep the named methods, move only the URL constants out.** Cosmetic. `Fetcher` would still enumerate the supported catalogs, and a third source would still mean a new method on a shared interface.

**Leave the parsers in `catalog`, move only the endpoints.** Tempting — a parser is arguably a format library rather than a source, and this is closest to ADR-0003's literal wording about "parsed shapes for the two catalog formats". Rejected because the package would still have to be edited for a driver with a new format, and the file map would still read as a list of blessed catalogs. Formats are not more universal than endpoints; both belong to whoever reads them.

**Generic `Fetch[T]` returning parsed values.** Go generics would let the transport return `T` instead of `[]byte`, saving each driver one parse call. Rejected: the parse must run twice anyway (once to validate a live body, once on the cached fallback), a generic method cannot be part of a non-generic interface without a type parameter on the interface itself, and the saving is one line per driver.
