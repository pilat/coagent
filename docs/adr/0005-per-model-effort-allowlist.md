# ADR-0005: Reasoning effort levels come from the catalog, per model

- **Status:** Accepted
- **Date:** 2026-08-07

## Context

[ADR-0003](0003-external-model-catalogs.md) moved model metadata to external catalogs but kept effort as a fixed trio: `low` / `medium` / `high`, hardcoded in the picker keyboard, in the session's validation, and in the budget-fraction map, with `medium` as the reset value on every model switch. The catalog was consulted only for a boolean — does this model reason at all.

That boolean is not what the catalogs actually publish. OpenRouter's `/api/v1/models` carries a per-model `reasoning` object with `supported_efforts`, `default_effort` and `mandatory`; models.dev carries `reasoning_options[].values`. Both describe **subsets of a seven-level vocabulary** (`none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`), and the subsets differ widely: a survey of the live OpenRouter catalog found 20 distinct level sets across 125 models that declare one, and models.dev 44 distinct sets. Concretely, on the models this daemon runs: `z-ai/glm-5.2` accepts only `xhigh` and `high`, `moonshotai/kimi-k3` only `max`, `high` and `low`, and four others (`minimax/minimax-m2.5`, `minimax/minimax-m2.7`, `xiaomi/mimo-v2.5-pro`, `moonshotai/kimi-k2.5`) declare no effort selector at all. Meanwhile `claude-*-4-6` accepts a `max` the trio never offered.

OpenRouter does not reject an unlisted level — verified against the live API, all nine probe combinations returned HTTP 200 — it documents that it "will map your requested effort to the nearest supported level". So the trio was never broken in the sense of failing requests. It was **lying**: the picker rendered three buttons for `glm-5.2` of which two collapsed onto the same `high`, offered an effort step for four models that expose no such control, and reset every switch to a `medium` that two models do not have.

## Decision

The effort allowlist is catalog data, per model, on the same footing as context window and pricing.

`ReasoningSpec` carries the catalog's own list (`Efforts`), its preferred level (`Default`), and a distinct `AnyEffort` flag for OpenRouter's documented "no allowlist, everything accepted" signal — which is `supported_efforts: null` and must not be confused with the key being absent, meaning the opposite (no effort selection at all). Since both decode to a nil slice in Go, the field is parsed as `json.RawMessage`.

Enrichment narrows that catalog list to what the driver can actually deliver and stores it on `ModelEntry.EffortLevels` (weakest first) plus `ModelEntry.DefaultEffort`. The narrowing is driver-specific: the native Anthropic driver takes the catalog list when the model has native effort, and the three levels its budget mapping covers when the model is budget-based (the choice is real there even though the catalog names no levels); OpenRouter takes its per-model list; every other driver takes none.

Everything downstream reads that list rather than a constant. The picker renders one button per level. Session validation rejects a level outside the model's list, naming what it does accept. A model switch resets to the model's own `DefaultEffort` — the catalog's preference when it has one, `medium` when on offer, else the middle of the list — instead of a global `medium`. A model with an empty list carries **no level at all**, and the store writes that empty level verbatim rather than coercing it back to `medium`.

Before a level goes on the wire it is clamped to the model's allowlist (`catalog.ClampEffort`, ties resolving to the weaker level), and a model that declares no effort selector gets no `reasoning` field at all.

## Consequences

- The picker tells the truth: `glm-5.2` shows two buttons, `kimi-k3` three, `claude-opus-4.6` four including `max`, and the four selector-less models show none.
- Effort levels became a seven-value vocabulary. `xhigh` and `max` are now reachable, which retires the "no xhigh/max" scope line from the original plan; the budget-fraction map gained `minimal` (0.1), `xhigh` and `max` (0.95).
- Clamping is belt-and-braces: the picker cannot produce an invalid level, so the clamp only catches levels persisted before a model switch. It stays because the gateway's own remapping is invisible, and doing it ourselves makes the behavior inspectable.
- Two sources must agree on one vocabulary. `catalog.SortEfforts` normalizes both into canonical weakest-first order and drops anything outside the known seven — a genuinely new gateway level is silently ignored until the vocabulary is extended. Accepted: the alternative is rendering a button whose semantics we do not know.
- An empty reasoning level is now a legitimate persisted state, not a bug to be defaulted away. The read path no longer invents `medium` either, so a stored empty level survives a reload.

## Alternatives Considered

**Keep the fixed trio and let OpenRouter remap.** Requests would keep succeeding, and this is the smallest possible change. Rejected because the failure is a UI lie rather than an error: the user picks `low` on `glm-5.2`, gets `high`, and nothing anywhere says so. It also cannot express `max` on the Anthropic models, which is a level the user pays for and cannot reach.

**Clamp only, without changing the picker.** Half the fix — the wire would be correct while the keyboard still showed dead buttons and the header still claimed a level the model never ran at.

**Derive the allowlist from a hardcoded per-model table.** Exactly the drift ADR-0003 removed, one field later.

**Treat absent `supported_efforts` as "everything accepted".** Tempting, since it collapses two cases into one and no live model currently uses `null`. Rejected because OpenRouter documents them as opposites, and guessing wrong means offering an effort control for a model that has none — the original bug, reintroduced.
