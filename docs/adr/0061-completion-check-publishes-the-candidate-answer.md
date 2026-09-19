# ADR-0061: The completion check publishes the candidate answer, not the confirming response

- **Status:** Accepted
- **Date:** 2026-09-18
- **Amends:** [ADR-0060](0060-wake-aware-model-completion-check.md)

## Context

ADR-0060 established the wake-aware two-phase completion check. Without a durable
wake source, a non-empty no-tool `stop` is stored as a hidden assistant
*candidate*, a host-authored nudge asks the model to continue with tools or
"respond once more with a concise explanation of why it is stopping," and the
next accepted stop confirms completion. ADR-0060 decided that the candidate is
"never human-visible" and that "only the confirmed response releases the original
manager input or becomes a subagent result" (its Decision and Consequences).

Production showed this specific grain loses the answer. When the model has
already given its full, considered answer as the candidate, the nudge — which
asks for a *concise explanation of why it is stopping* — elicits a terse
restatement. That terse restatement is the confirmed response, and it is what the
user (via the manager) or the parent (as the subagent result) receives; the full
candidate answer is discarded and never surfaces. The two-phase mechanism itself
is correct and remains. What was wrong is narrow: which text is treated as the
answer, and whether the candidate is visible.

## Decision

The published answer is the **candidate** — the first substantive stop — not the
confirming response. The candidate becomes human-visible; the confirming
response's text is discarded and serves only as the durable second-look signal.

- **Managed root:** while the candidate is pending, its text is the current
  progress-card note (`LatestModelProgress`), so the live `🟢`/`🟣` card shows the
  full answer as honest in-progress narration — never "your turn." On
  confirmation the candidate's text is published as the releasing persistent
  final; because the nudge does not advance the model-input generation
  (ADR-0035), that final reuses the same replaceable progress card's receipt and
  freezes it in place (best-effort — a fresh bubble when no card preceded), the
  badge dropping to the badge-less "your turn" state. No receipt-layer change.
- **Subagent:** a durable nullable column `completion_check_confirmed_answer_id`
  records the confirmed candidate's message id in the same transaction that
  clears the candidate; external model-visible input clears it alongside the
  candidate; result derivation returns that message's content instead of the last
  assistant text.
- The nudge wording is unchanged: its response must be a text stop (not empty) to
  keep empty-stop routing intact. We keep the response; we simply do not treat it
  as the answer.

This reverses ADR-0060's "candidate never human-visible / confirmed response is
the answer" for both the manager-output and the subagent-result boundaries, while
preserving ADR-0060's two-phase second look, wake-awareness, empty-stop streak,
manager-reply obligation, and deterministic restart recovery.

## Consequences

- The user and a parent agent receive the model's full considered answer, not a
  terse "why I'm stopping" restatement.
- The candidate is now human-visible. On **continue** it appears ephemerally on
  the working card and is rolled over by later progress — a candidate the model
  outgrew by continuing is progress, not a final. On **confirm** it is the
  permanent final.
- A confirming response that rarely carries a genuinely new thought is lost.
  Accepted: the candidate is the full considered answer and the confirming
  response is, by construction, an explanation of why it is stopping.
- Budget-fired confirm diverges by boundary: the root drops the candidate (its
  outbox row is suppressed after budget fires), while a subagent still recovers
  it through the pointer. Benign — strictly better for children, no stale-pointer
  risk.
- New durable state: one nullable session column, set on confirm, cleared on
  external input, read at finalization. It must be preserved and cleared by the
  same rules as the candidate column; process-local fields are never recovery
  authority.
- No new receipt-layer machinery. Correctness of the in-place edit rests on the
  existing replaceable→persistent reuse and the model-input generation boundary
  (ADR-0035); the durable answer rests on immutable message history
  (ADR-0035/0056 — compaction never rewrites `content`).

## Alternatives Considered

- **Concatenate the candidate and the confirming response.** Rejected: under the
  linear single-slot receipt model the confirming card would replace the
  candidate rather than append, or would demand addressed receipt edits; and once
  the full candidate is shown, the "why I'm stopping" ack is redundant.
- **Deliver the candidate immediately as its own persistent bubble.** Rejected:
  at delivery time the outcome (confirm vs continue) is unknown, so a badge-less
  "your turn" bubble desyncs the moment the model continues, and
  continue→continue accumulates frozen intermediate bubbles.
- **Deliver the candidate as a replaceable message.** Rejected: the next progress
  card clobbers it within seconds by reusing its receipt — the original bug.
- **Addressed receipt edit** (deliver provisionally, edit that exact message in
  place once the outcome is known). Rejected: it breaks the intentionally linear
  single-slot receipt layer, needs a second slot and a durable race against the
  reconciler — disproportionate to a few seconds of desync.
- **Reconstruct the subagent's candidate by walking the transcript past the
  nudge.** Rejected: the nudge is a `user`-role row the loop must not recognize
  by text, and subagents receive other genuine `user`-role rows (promoted inbox
  completions), so "trailing user row is the nudge" is false and the candidate
  sits unreachable behind the nudge barrier. A durable pointer is used instead.
- **Flag the answer on the `messages` row (`is_answer`), no clearing needed.**
  Rejected: the "no clearing" simplification is unsafe. The result-derivation's
  completed branch is reached not only after a confirm but after a
  background-yield final (which sets no flag), so after a prior confirm (flag on
  A) plus a re-activation that ends in a yield, "last flagged" returns the stale A
  instead of the yield text. Making it correct requires clearing on external
  input anyway — the pointer's exact cost — and additionally introduces a second
  kind of post-insert mutation on otherwise-immutable message rows and scatters
  completion state onto `messages`. The session pointer is additive
  (consulted only when set, else the existing last-assistant-text derivation
  stands, preserving yield-final results) and keeps completion state beside its
  siblings on `sessions`.
- **Rewrite the nudge so its response is never worth keeping.** Rejected as the
  primary fix: publishing the candidate directly is simpler and loses nothing,
  and the wording must stay because an empty confirming stop would misroute into
  the empty-stop streak.
