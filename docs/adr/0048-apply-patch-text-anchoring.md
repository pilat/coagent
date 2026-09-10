# ADR-0048: apply_patch anchors on text, not line numbers

- **Status:** Accepted
- **Date:** 2026-09-08

## Context

`apply_patch` applied hunks positionally: it read `@@ -OldStart,OldCount` from the
hunk header, cut those line numbers out of the file, and spliced the new lines in,
without ever checking that the patch's context (` `) and removed (`-`) lines match
what is actually at those positions. If the file shifted — an unrelated insertion
above, or a slightly wrong line number from the model — the tool cut the wrong
region and corrupted the file with no error. It also wrote each file as it looped,
leaving earlier files written when a later hunk failed.

This matters more now that the read-before-write guard (ADR-0047) deliberately
leaves `apply_patch` unguarded, trusting it to fail cleanly instead of corrupting.
That trust only holds if the patch is anchored to the text it claims to touch.
OpenAI Codex's `apply_patch` demonstrates the approach: locate hunks by matching
context, not by line number.

## Decision

We rewrite the application engine to anchor on text while keeping the unified-diff
wire format. For each hunk we build an old-block (context + removed lines) and a
new-block (context + added lines), reusing `edit`'s exact-substring locate
primitive. Matching is: exact, then one retry with per-line trailing whitespace
stripped; 0 matches fails the whole patch; multiple matches are disambiguated by
the `@@ -N` line number as a tiebreaker (nearest occurrence in the running
content), and a true tie fails. The `@@` line number is a hint only, never the
source of truth.

Application is all-or-nothing: parse the whole patch, build every target file's
new content in memory, and write nothing unless every hunk in every file located.
This means "no failure-triggered partial writes" — not crash-atomicity across
files, since each file is a separate write. The per-file post-write read-back
verification stays.

## Consequences

- A patch with correct context but stale line numbers now applies at the right
  place; a patch whose context no longer exists fails loudly instead of
  corrupting — the property ADR-0047 depends on.
- A failing multi-file patch no longer leaves earlier files half-written (barring
  a process crash mid-write).
- Failures are more frequent than positional splicing (a mismatch that used to
  corrupt now errors), which is the intended trade — the error nudges a re-read.
- New logic beyond the reused primitive: the whitespace fallback, the nearest-hint
  disambiguation, and multi-file locking (all target locks acquired in sorted
  order, held across locate→write).

## Alternatives Considered

- **Keep positional splicing.** Rejected — it is the corruption source and is
  incompatible with leaving `apply_patch` unguarded.
- **Fuzzy / AST-based matching.** Rejected — unpredictable, and unnecessary when
  the model can supply exact context; exact-plus-trailing-whitespace is trustworthy
  without guessing.
- **Adopt Codex's V4A patch format.** Rejected — churns the wire format the model
  already produces; only the application engine needed fixing.
- **Ignore the `@@` line number entirely.** Rejected — it is a cheap, reliable
  tiebreaker when the same context appears more than once.
