# ADR-0102: A sleep within the in-turn wait limit waits inside the turn

- **Status:** Accepted
- **Date:** 2026-11-20
- **Amends:** [ADR-0091](0091-browser-runs-on-a-fixed-model-with-granted-built-ins.md)

## Context

Every `sleep` has been a durable suspension. The tool writes a one-shot schedule row that owns the call's
result and returns `ErrSuspend`. The runner exits, and the scheduler, which ticks every 10 seconds, delivers
the result through the inbox and reopens the session.

That is right for minutes to days. For a pause of a few seconds it is wrong three ways:

- The wake comes up to 10 s late, so a 3 s sleep lasts 3 to 13 s.
- It costs a second model round: the calls the model put after the sleep in the same response are skipped and
  must be asked for again.
- The reopened session starts a fresh loop detector.

Bash already has one idea of "short enough to wait for": a foreground command runs inline for 10 seconds
before it moves to the background. A second, different limit for sleep would give the human two ideas of
short.

## Decision

`tool.InTurnWaitLimit`, 10 seconds, is the one limit. Bash's foreground limit is that constant, and so is the
sleep threshold.

- A `sleep` whose duration is at most the limit, inclusive, waits on a timer inside its own call and returns
  `Sleep completed (<duration>).`. It writes no schedule row and suspends nothing. Because `sleep` is a barrier
  stage, the calls after it in the same response run after it. An RFC3339 time no further away than the limit
  counts as such a duration.
- A longer `sleep` is the durable sleep, unchanged: the same checks in the same order, the same texts, the same
  schedule row and the same suspension.
- The sleep guard covers both. While a background child or an advertised process owns a pending completion,
  every `sleep` is refused.
- The run context ends an in-turn sleep at once on `/stop`, `/kill`, a parent's kill or shutdown. A budget
  park does not cancel the run: it parks the session after the sleep's step.
- `browser.tools` accepts `sleep`. Nothing is granted to the browser by default.

## Consequences

- A short pause is exact and costs no extra model call. The prompt tells the model which sleeps wait inside the
  turn, so every session with `sleep` changes its prompt bytes once.
- A human message sent during an in-turn sleep does not interrupt it. The message is taken at the next step
  boundary, at most 10 s later, as during any long tool call; slash commands resolved at the boundary wait the
  same way. `/stop`, `/kill` and `/status` still act at enqueue.
- Polling is bounded by what already existed: the sleep guard and the loop detector. Polling outside state with
  varied short sleeps is not bounded, just as with `bash sleep` before.
- Several short sleeps in one response run one after another and add up.
- `sleep` stays a pending external call by name (`tool.IsExternalCall`). A restart during an in-turn sleep
  therefore settles it through the orphaned external call path of
  [ADR-0016](0016-boot-time-cancellation-of-unowned-external-calls.md), with that path's producer wording.
  `batch` still refuses it as a call that "suspends the session".
- An in-turn sleep is never a pending sleep. The waiting projection, `/status` and the `⏰` marker show the
  session as working.
- Changing the limit changes bash's foreground limit too. Tests fail until the numbers in both tools' texts and
  in the prompt follow.

## Alternatives Considered

- **A separate in-turn variant of `sleep` registered only for frame sessions** (about 106 added lines).
  Rejected: two implementations behind one name, a prompt trait to tell them apart, and only one session type
  gets an exact pause.
- **A new in-turn `wait` tool** (about 85 lines). Rejected: a second model-facing name for a pause, while every
  other session still pays the durable path for a few seconds.
- **Returning early from an in-turn sleep when input arrives** (about 40 more lines). Rejected: a new
  runner-to-tool signal across packages, for a window of at most 10 s.
- **Two constants, each with a comment to keep them equal.** Rejected: bash and the sleep tool already depend on
  `internal/tool`, so one shared constant costs no dependency edge and cannot drift.
- **Keeping some session types durable-only**, to keep polling expensive. Rejected: it brings back a branch per
  session type, and the guard still refuses the polling the tool texts forbid.
