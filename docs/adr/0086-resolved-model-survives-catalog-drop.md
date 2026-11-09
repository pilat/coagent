# ADR-0086: A model that resolved before survives losing its catalog entry

- **Status:** Accepted
- **Date:** 2026-11-09
- **Amends:** [ADR-0003](0003-external-model-catalogs.md)

## Context

ADR-0003 made the catalog the only source of model metadata and accepted a consequence: a model
removed from its catalog turns a working config into a startup failure once the cache refreshes. On
2026-11-08 OpenRouter removed a configured model. The daemon had run with it for days, because the
catalog is consulted only at boot. The next restart crash-looped until systemd gave up, and the only
repair was a hand edit of `config.yaml`. A daemon that cannot come back from a reboot because a vendor
edited a list is not unattended.

The typo case is still real: an id that never resolved must fail loudly. Both cases reach enrichment as
the same lookup miss, so telling them apart takes a memory that outlives a catalog refresh. The
fetcher's disk cache cannot be that memory: every good fetch overwrites the one snapshot for its URL
before any caller sees the body, so when enrichment misses the model the old body is already gone.

## Decision

We keep the last resolution for each (model, origin) pair in
`~/.coagent/cache/catalog/resolved-models.json`: the resolved spec, when a catalog last answered for it,
and a fingerprint of the provider's driver, catalog key and base URL. The file is rewritten atomically,
and only after a whole enrichment has passed validation.

When a catalog lacks a configured model and the file holds an entry with the same fingerprint, the
daemon starts on that spec. It logs one `catalog_model_missing_upstream` WARN per model at every boot,
marks the model `(stale catalog)` in `/model` and `coagent status`, and keeps it selectable. Nothing is
removed or replaced automatically: the operator decides. A carried spec passes the same validation as a
fresh one. Without such an entry the boot fails with ADR-0003's error, and a catalog that cannot be
obtained at all still fails as before.

## Consequences

- A vendor dropping a model no longer stops the daemon from booting.
- Coagent can run a model the vendor has retired. Calls then fail as provider errors after the normal
  retry policy, at first use instead of at boot. A tagged stale model stays on offer to `task`, so an
  autonomous spawn can hit this too.
- The WARN and the mark are the only signals, and nothing pushes them to the operator.
- A stale model's window and prices are frozen at the last resolution and may be wrong.
- The file is a cache, not durable state: no table, no migration. Losing it reinstates ADR-0003's
  failure for any model already gone from its catalog, and nothing can rebuild it.
- Protection starts at the first successful boot on this version. A model already gone before then has
  no entry.
- The catalog remains the only source of metadata. An entry is the catalog's own past answer, never an
  operator override.
- A whole models.dev section disappearing still fails boot for a provider pinned to it.
- Entries for models no longer configured are kept, so commenting a model out and back in does not read
  as a typo. The file is never pruned and is bounded by the (model, origin) pairs ever configured.
  Resolutions for earlier origins are kept because a configuration can go back to an earlier provider.

## Alternatives Considered

- **Resolve against the previous cached catalog body.** A `.prev` file survives one restart: the next
  boot rotates a body that already lacks the model. Any crash loop burns the generation in seconds.
  Keeping the right body needs a fetcher that knows which ids matter, against its contract of knowing no
  payload format, or several bodies of up to 64 MiB. Every driver would also need a second path through
  `ListModels`, and a body's age says nothing about when one model was last seen.
- **A SQLite table.** A migration for data whose loss is harmless, and enrichment runs before the
  database opens.
- **Key by model id alone, with no fingerprint.** A provider re-pointed at another endpoint would inherit
  what a different catalog said about a model the new one has never heard of.
- **Keep failing and improve the message.** The failure is the problem, not its wording.
- **Fall back to another configured model.** It silently changes what a session runs on and what it
  costs. That is the operator's decision.
- **Accept unknown models with guessed metadata.** This is the second source of truth ADR-0003 removed,
  and it erases the typo check.
