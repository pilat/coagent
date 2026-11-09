# ADR-0087: A catalog section that disappears is a lost entry for each of its models

- **Status:** Accepted
- **Date:** 2026-11-09
- **Amends:** [ADR-0086](0086-resolved-model-survives-catalog-drop.md)

## Context

ADR-0086 lets a model its catalog no longer lists keep its last resolution, and it records one gap: a whole
models.dev section disappearing still fails boot for a provider that reads it. Such a provider either pins the
section with its `catalog` key or reads one implicitly, as the `anthropic` and `google-sa` drivers read `anthropic`
and `google-vertex`. When a fetched body lacks the section, every such boot fails with
`catalog unavailable: models.dev has no section "<name>"`, a crash loop. The catalog did answer; the error only reads
like an outage. The body cache cannot help, because the body that lacks the section has already replaced the cached
one. A section name that never existed is a typo and must keep failing loudly, and enrichment receives both as the
same error.

## Decision

A models.dev body that parses but lacks a provider's section counts as a catalog that lists none of that provider's
models. Each of them then takes ADR-0086's path: a model with a last resolution for its (model, origin) pair boots on
it, warns `catalog_model_missing_upstream` at every boot and shows as `(stale catalog)`. If any of the provider's
models has no such resolution, the boot fails with the provider-level error it reports today, unchanged. A section
that nothing vouches for may be a typo, and a model that never resolved gets exactly that message today. A catalog that cannot be obtained stays fatal, as ADR-0086 records.

## Consequences

- A section that models.dev removes or renames no longer stops a boot for models that resolved before.
- The WARN does not name the section. An operator sees a model "missing upstream" and finds the section gone by
  looking at the catalog.
- A section-name typo keeps today's message byte for byte. That includes re-pointing a resolved model at a section
  that never existed, because the origin fingerprint includes the catalog key.
- A renamed section leaves its models stale until the operator re-pins the provider to the new name.
- A body that parses but no longer holds the sections providers read reads as every such section lost. An example is
  sections moved under a new top-level wrapper. Models that resolved before then boot stale, under a WARN that
  blames upstream rather than the format. An `openai` provider without a `catalog` key already behaves this way.
- A boot that fails on a lost section can still log the WARN for that provider's carried models first, as the
  not-listed path already does.
- Only a missing section is told apart. Every other catalog failure still reports `catalog unavailable` and fails
  boot.

## Alternatives Considered

- **Return an empty section instead of an error.** It is the smallest change, but the typo case would then report
  each model as `not present in its catalog` and lose the fact that the section name is wrong.
- **Carry a model on every catalog error.** An unobtainable catalog would become a stale boot, which ADR-0086 keeps
  fatal, and the WARN and the mark would claim the vendor dropped a model when the catalog was only unreachable.
- **Make each driver report whether the section exists.** That changes the contract of all four drivers for one
  caller's distinction. A wrapped sentinel error carries the same fact with no interface change.
- **Keep failing.** The catalog answered. Failing here is the crash loop ADR-0086 exists to prevent.
