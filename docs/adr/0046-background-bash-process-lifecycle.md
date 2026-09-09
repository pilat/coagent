# ADR-0046: Background Bash processes use durable completion events

- **Status:** Superseded by [ADR-0050](0050-asynchronous-completions-use-session-inbox.md)
- **Date:** 2026-09-07

## Context

Bash currently waits synchronously for up to two minutes by default and ten
minutes when requested. A healthy long-running build or test therefore blocks
its session, while timeout kills the command and is returned as an ordinary
successful tool result. Models work around this with `nohup`, redirected output,
`ps`, `tail`, and sleep loops. Those workarounds lose process ownership and turn
waiting into repeated model calls carrying the complete conversation context.

Interactive coding agents commonly return a handle for a long-running command,
but pull-based output APIs still encourage model polling. Coagent already has a
stronger pattern for background subagents: work can outlive one activation, its
completion is delivered durably, and the appropriate session wakes without
polling. Bash needs the same behavior while preserving arbitrary non-interactive
shell commands and bounded local resource use.

## Decision

Bash waits in the foreground for ten seconds unless the model requests immediate
background execution. A command still running then becomes a session-owned
background process: the Bash tool call returns a daemon-minted process ID and an
absolute output-file path, and a later durable `process_event` wakes a session on
terminal completion. The model-provided timeout is the complete process
deadline, defaulting to ten minutes and capped at thirty minutes.
The wire value remains milliseconds; omitted and nonpositive values choose the
default.

Each exact root or subagent session owns at most four live Bash processes. There
is no daemon-wide process pool or aggregate admission limit. One raw combined
stdout/stderr file under `~/.coagent/processes/<session-id>/` captures output from
process start. A per-process 100 MiB limit kills the process group with an
`output_limit_exceeded` outcome. Small foreground commands retain bounded inline
results and remove the unadvertised candidate file; advertised or spilled output
remains file-backed. A generic `tail` tool reads small suffixes; complete output
never enters the transcript.

Completion wakes the root or a currently active/suspended owning subagent. If
the owner is a terminal subagent, completion wakes the root and identifies the
subagent and output path; only an explicit `send_to_subagent` starts another
subagent round. Natural activation completion leaves processes running, while
explicit stop and kill cancel and join every process in the root tree and
suppress cancellation wake events.

Controlled daemon shutdown and restart cancel and join owned process groups,
record advertised work interrupted, and leave its completion event owed for the
next startup.
After an abrupt daemon death, startup marks leftover durable operations
`interrupted`, preserves their partial output, and delivers one event without
signalling a stored PID. The first version does not add a per-process guardian:
an unconfined quiet child may survive daemon `SIGKILL`, panic, or OOM even though
its coagent operation is terminalized as interrupted.

## Consequences

- Long builds and tests release the session after ten seconds and later resume it
  from one completion event instead of consuming model turns through polling.
- Models can continue independent work or finish an activation while a command
  runs, and another session can inspect the named output file when it has normal
  filesystem access.
- Timeout, nonzero exit, output overflow, interruption, and cancellation become
  structured failures rather than successful text that downstream scheduling
  must interpret.
- Per-session slots and per-process output/deadline limits bound one session's
  mistakes. Concurrent sessions remain intentionally ungoverned in aggregate,
  and output retention has no automatic cleanup policy.
- Completion routing, stop fencing, startup recovery, output collection, and
  process joining become durable temporal protocols requiring scenario tests.
- Abrupt daemon death does not prove child-process quiescence. If operational
  evidence makes this material, add a per-process guardian that parents the
  command, monitors an inherited daemon lease, kills and joins on EOF, and lets a
  new daemon wait on guardian quiescence without trusting PID reuse.

## Alternatives Considered

- **Keep synchronous Bash and raise its timeout.** Rejected because it leaves the
  session blocked and encourages repeated oversized model calls whenever the
  timeout is guessed incorrectly.
- **Return a process handle and require status/output polling.** Rejected because
  pull-only waiting is the failure observed in current agents: every empty poll
  costs another model iteration. Completion is pushed automatically; the output
  path remains available for optional inspection.
- **Suspend the session until the process finishes.** Rejected because the model
  may have independent work and the current activation may finish legitimately
  before the command.
- **Add a verification-specific command tool.** Rejected because arbitrary
  project gates have no portable protocol. Bash lifecycle and typed outcomes are
  the reusable boundary.
- **Keep stdout and stderr in memory or separate files.** Rejected because memory
  capture does not support large/live output and separate files lose the
  observed terminal order. One bounded combined file is simpler to inspect and
  hand to another session.
- **Add daemon-wide process and disk quotas.** Rejected for this version. Limits
  belong to each owning session/process; aggregate resource governance is a
  separate operational policy.
- **Add a per-process guardian immediately.** Deferred because one extra process,
  internal IPC, lease handling, and crash races are disproportionate to the
  initial risk. The accepted limitation is explicit so a future implementation
  can add the guardian without rediscovering the tradeoff.
