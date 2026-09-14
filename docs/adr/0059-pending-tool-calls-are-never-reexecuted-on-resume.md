# ADR-0059: Pending tool calls are never re-executed on resume

- **Status:** Accepted
- **Date:** 2026-09-14

## Context

Sessions persist to SQLite and resume automatically after a daemon restart.
`handlePreviousResult` executes pending non-external tool calls left in the
transcript by a crash — the assistant message with the `tool_use` survived, the
result did not. After a restart this silently re-executed every pending call:
a `bash` command half-run, an `apply_patch` half-applied, a `write` half-written
were all run again from the start, and the model saw a fresh result as if the
operation ran once. The restart may happen long after the session was
interrupted, which makes the world the operation would act on arbitrarily stale.

External calls (`task`, `sleep`, config/secret) are exempt from this problem:
they suspend the loop and are answered on resume by their durable producer or
the orphaned-call pass (ADR-0016), never re-executed.

## Decision

Every pending in-loop (non-external) tool call is **settled as a typed failure
on resume** — `bash`, `apply_patch`, `write`, `edit`, `read`, `grep`, and any
other tool whose call the loop would otherwise re-execute. The model sees that
the call was interrupted before completion, was not re-executed, and may have
partially completed; it checks the current state and explicitly re-executes
what it still needs.

This applies whether or not the operation actually started before the crash: a
crash before spawn also leaves the outcome unknown, so both cases resolve
identically as failures.

The settlement lives in the daemon's boot sweep (PASS 0, alongside the
orphaned external-call pass), not the session loop, because the loop cannot
distinguish a stale call left by a restart from a fresh call the model just
made: both appear as pending in the same transcript shape. The sweep settles
every unresolved in-loop call before any session resumes, so the loop never
sees a stale call and its normal execution path is untouched.

## Consequences

- The model is always told its pending call was interrupted; no silent
  re-execution of any tool after a restart, and no duplicated side effects.
- The model must explicitly retry an interrupted call, costing an extra LLM
  turn even for read-only calls like `read` or `grep`.
- The failure notice is generic and does not carry the interrupted process's
  output-file path (unadvertised foreground processes are invisible to the
  model anyway); the model investigates or retries as it sees fit.
- No database schema change: the `background_processes` ledger, its columns,
  and the session inbox are unchanged and fully used.
- The boot sweep opens each candidate session once to settle its stale calls,
  mirroring the orphaned external-call pass; sessions without stale calls are
  untouched.

## Alternatives Considered

- **Re-execute and notify.** Keep automatic re-execution but tell the model the
  first attempt died. Rejected: the side effects are already duplicated by the
  time the model reads the notice; the damage the decision exists to prevent
  has happened.
- **Exempt read-only calls from settlement** (re-execute `read`/`grep`/`glob`
  and settle only side-effectful tools). Rejected: it draws a line the loop
  cannot enforce — any tool may carry side effects through MCP or future
  built-ins — and a stale read result is still a result the model never saw
  produced. Uniform settlement is simpler and safer.
- **Skip settlement when no process row exists** (crash before spawn), and
  re-execute in that case. Rejected: a pre-spawn crash still has unknown
  outcome, and consulting the process ledger from the sweep couples packages
  for no safety gain. Both cases resolve as failures.
- **Settle stale calls inside `handlePreviousResult`.** Rejected: the loop runs
  on every iteration, and a fresh tool call the model just made is pending in
  the same transcript shape as a stale one — settling there would fail the
  model's own calls, not just the interrupted ones. Only the boot sweep knows
  which calls predate the restart.