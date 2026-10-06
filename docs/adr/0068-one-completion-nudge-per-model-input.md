# ADR-0068: One completion nudge per model input

- **Status:** Accepted
- **Date:** 2026-10-05
- **Amends:** [ADR-0060](0060-wake-aware-model-completion-check.md)

## Context

ADR-0060 makes a non-empty no-tool `stop` without a wake source a hidden
candidate and appends a host nudge: continue with tools, or answer once more with
a concise explanation of why you are stopping. It also decided that "tool use or
fresh external input invalidates the old confirmation and makes a later stop
begin a new check." ADR-0061 then publishes the candidate's text instead of the
confirming response.

The first browser subagent runs showed the cost of "a later stop begins a new
check". Session 726 answered, received the nudge, made one more browser action,
answered again, received the nudge again, and so on. It reached fourteen nudges
and was still running. Each answer after a continuation was a delta ("the
Boardroom FAQ confirms…") rather than the full answer, because the model treated
its earlier answer as already delivered. Whichever delta came last would have
become the child's result. Nothing in the protocol bounds this loop: a model that
reads "re-check" as "do one more thing" keeps the session alive indefinitely.

## Decision

At most one completion nudge is issued per model-input generation (ADR-0035). The
host records, durably and in the same transaction as the nudge, the generation in
which it nudged. Within that generation, any later non-empty no-tool `stop` is
accepted as final immediately. Its own text is published, because the tool calls
that followed the nudge invalidated the old candidate. A subagent's result
derives from the last assistant text, as it does without a confirmed-answer
pointer.

A new generation, created by each genuine model-bound input (user or manager
message, follow-up to a subagent, promoted completion), restores the nudge. Tool
results do not reset it, and neither does the nudge itself, since neither
advances the generation.

The nudge text keeps its ADR-0061 shape. It gains one sentence: if the model
continues with tools, its next text-only response ends the task without another
check and replaces this answer, so it must be complete.

Everything else in ADR-0060 and ADR-0061 stands: wake-aware yield, the empty-stop
streak, the manager-reply obligation, publishing the candidate on a direct
confirmation, restart recovery.

## Consequences

- The completion check costs at most one extra model call per input, and a
  continue-loop is impossible by construction.
- After a continuation there is no second look. A model that stops too early the
  second time is believed. ADR-0060's premature-stop problem returns, but only
  after one explicit re-check.
- Whether the final answer is complete after a continuation depends on the prompt
  and is not guaranteed. The model sees its earlier full answer in history and is
  told the next one replaces it.
- A session resumed without new input (after a stop or a daemon restart) stays in
  the same generation, so if it already nudged, its next stop is final.
- New durable state: one nullable column on `sessions`. It is compared against
  `model_input_generation` and never cleared, so no invalidation path can forget
  it.

## Alternatives Considered

- **Keep re-checking after every continuation (ADR-0060 as written).** Rejected:
  it produced the unbounded loop and fragment answers above.
- **Skip the nudge only in browser sessions.** Rejected: any agent type whose
  model answers "re-check" with an action loops the same way.
- **Concatenate the old candidate and the new answer.** Rejected for the reason
  ADR-0061 gives: the root's linear single-slot receipt model replaces rather than
  appends, and the subagent path would publish duplicated text.
- **Cap nudges at N > 1 per input.** Rejected: one re-check is the point of the
  check, and every further one in 726 produced only another fragment.
- **Detect an earlier nudge by its row text.** Rejected: ADR-0061 forbids
  recognizing the nudge by text, and process-local memory is not recovery
  authority.
