# ADR-0034: Vision via referenced tool-result attachments

- **Status:** Accepted
- **Date:** 2026-08-27

## Context

The pipeline could not show any image to any model: `llmwire.Message.Content` is a plain
string, both driver families serialize only text/tool blocks, and `read` rejects binaries
including every image extension. Multi-image user input therefore had no path into context.
Providers are stateless, so peer agent implementations carry real pixels through conversation
history; they differ only in where those pixels are stored (inline data URLs / temp files with
placeholders) and how pressure evicts them.

Coagent's append-only context log projects "what the model sees" at load time. Entering pixels
into that log as blobs would duplicate megabytes into SQLite and recreate an image-eviction-budget problem; skipping images entirely would leave models blind to legitimate
visual input. Telegram simultaneously gained file ingestion, which produces files on disk that
models should be able to inspect on demand without any of them being forced into context wholesale
(a full PDF dumped into a request was an explicitly rejected outcome).

## Decision

We store **references** (`{Path, Mime, Size}`), never pixel bytes, in one new JSON column on
`messages` following the `reasoning_raw` sealed-envelope precedent, populated only by role-tool
rows produced through `read`. Every provider request re-materializes references to base64 blocks
inside each driver's conversion step; anything unmaterializable (missing file, non-image mime,
model without image input modality) degrades to an inline text placeholder instead of erroring,
with wording owned centrally so it cannot drift between drivers. The capability gate is
fail-closed from catalog-provided modality data (`modalities.input` arrays on models.dev, arrow
form `architecture.modality` on OpenRouter); unknown modality information means no image is sent.

Token estimation counts each reference as `min(Size/4, 8192)` — bounded above because providers
downscale oversized images themselves (long edge ≤1568px bounds real cost near a few thousand
tokens), so proportional-only accounting would trigger spurious destructive auto-compaction on
ordinary photos. The single compaction point of ADR-0013 remains the only pressure response;
no image-eviction budget exists. `read` accepts {jpeg, png, gif, webp} up to 5 MB — the strictest
per-image limit across coagent's driver families (Vertex/Bedrock) — so no accepted image can be
rejected by a provider and then replay-poison the session every turn afterward. Manager boundaries
stay strings-only: Telegram ingestion writes files to `/tmp` with random names and delivers a
fixed-EN metadata text whose only conditional advice points small images at `read`.

## Consequences

- A cleared/compacted/reaped turn loses its pixels gracefully (placeholder) — prompt-cache loss
  is local to the changed message onward; the projection now depends on disk state, not only on
  stored rows. Byte-stable projections can be restored later by filling the same column with
  bytes, a local change.
- Multi-turn conversations re-upload base64 per request (stateless APIs); acceptable ≤5 MB, named
  as a Files-API-shaped future improvement if it bites.
- `/status` occupancy for image-bearing sessions is approximate (pixel dimensions invisible to the
  estimator); the ceiling errs conservative against undercounting.
- Vertex/OpenRouter acceptance of image content inside role-tool arrays is asserted from SDK types
  but verified live only for a pre-merge manual smoke; first google-sa image session must follow it.
- Loop-detector fingerprints require distinguishing success text (resolved paths embedded).
- No native PDF/document blocks, no resize/detail budgets, no upload-attached media — all recorded
  as revisit-on-evidence items rather than lost decisions.

## Alternatives Considered

- **Store base64 inline in SQLite** (inline data-URL style). Rejected: duplicated megabytes per row,
  slower loads/backups, plus their compaction eviction machinery; reversible later since the column
  format can carry bytes.
- **Attach uploads directly via extended controllerapi DTOs.** Rejected after transport analysis:
  manager→transcript entries have only ever been strings (`SessionMessageData`, `sessionInput`
  variants); typed media crossing that boundary buys one saved tool roundtrip while creating the
  first non-string contract surface. `read` covers visibility; upload messages stay metadata.
- **Separate `view_image` command.** Rejected: peer implementations use the ordinary read path;
  a second reading tool splits MCP/tool budgets and duplicates binary-sniff logic.
- **Provider-side rescaling/budget pass before send** (per-request detail budgets). Rejected for v1:
  any transform adds cross-turn determinism obligations against prefix caching; provider-side
  downscaling already bounds cost, and failures will arrive as evidence if needed.
