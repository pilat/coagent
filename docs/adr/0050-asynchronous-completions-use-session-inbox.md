# ADR-0050: Asynchronous completions use the session inbox

- **Status:** Superseded by [ADR-0053](0053-background-handoff-uses-ordinary-completion.md)
- **Date:** 2026-09-09

## Context

[ADR-0046](0046-background-bash-process-lifecycle.md) introduced a durable
delivery state machine for background Bash completion. A terminal process first
claimed a target session, then an in-memory coordinator attempted transcript
injection, and a retry worker retained the producer obligation until a separate
acknowledgement updated the process row.

Production use exposed a mismatch between that mechanism and the session loop.
The coordinator could enqueue into a live runner, but the runner drained that
queue only outside a complete `RunDaemon` activation. A model performing useful
work or polling inside one long ReAct loop therefore never observed completion.
When its root was explicitly stopped, claimed events were repeatedly routed to a
session that correctly refused to run, turning a permanent lifecycle state into
an infinite retry. Background subagent results used a different producer ledger
but suffered the same mid-loop visibility problem.

The daemon already has a durable FIFO that solves the relevant ordering and
recovery problem for user and agent input: `session_inbox`. The shared session
loop accepts that input only after the current assistant/tool-result batch is
durable and before the next model request. Reusing that boundary avoids both
concurrent transcript mutation and a second delivery state machine.

## Decision

Independent asynchronous completion enters a session through
`session_inbox`. Its producer vocabulary distinguishes `user`, `agent`,
`process`, and `subagent`. Process and background-subagent rows become bounded,
explicitly tagged `role=user` turns when the shared loop accepts them. They do
not synthesize assistant tool calls and never create manager output directly.
Exact external results, including foreground subagent completion, retain their
existing tool-call-bound protocol and causal priority.

A background process commits terminal lifecycle state and its process-source
inbox row in one transaction. The inbox insert is the producer acknowledgement;
`background_processes` has no delivery target/state/acknowledgement columns and
there is no post-acknowledgement retry worker. A background subagent similarly
commits its delivered link state together with one subagent-source row in the
parent inbox. The existing activation sequence continues to reject stale rounds.

Session status decides whether durable input wakes work. Active and completed
sessions run, a genuine background wait resumes, and an interruptible sleep is
resolved before the independent event. Stopped and errored sessions retain the
row without running; the next explicit input consumes the FIFO. Stop preserves
facts committed before its durable intent and suppresses completions caused by
the stop itself. Kill and clear retain no pending asynchronous input.

Subagents use the same loop. A process completion input committed before child
terminalization keeps the current round open. If the prior round was already
delivered, the existing re-arm transition starts a new asynchronous round; a
foreground task call is never resolved twice. Normal child completion does not
kill the child's background process merely because the child session is idle.

Background Bash is daemon-lifetime work. A per-process lease guardian kills the
tracked process group when the daemon dies. Startup waits for guardian
quiescence, changes leftover running records to `interrupted`, and enqueues an
event only for processes that had returned a background handle. A process that
deliberately escapes its tracked group is an accepted limitation.

Process output is stored below a project-scoped subtree,
`~/.coagent/processes/project-<project-id>/`. A shields-down sandbox receives
only its current project's subtree as a writable root; the global processes
directory and other project subtrees are never granted.

## Consequences

- User messages, process completion, and background-subagent completion share
  one FIFO and one safe model-input boundary.
- A busy loop observes completion between ReAct rounds without waiting for the
  complete activation to return.
- Durable handoff has one acknowledgement point. A best-effort wake may be lost
  or duplicated without losing or duplicating the inbox row.
- Stopped and errored sessions accumulate completed facts without violating the
  operator's decision not to run them.
- Process finalization now owns a cross-table transaction with session input,
  and background subagent delivery changes its cross-table destination from
  parent messages to parent inbox.
- Process lifetime requires a real helper process, handshake, lease, group kill,
  and restart-quiescence test on supported platforms.
- Completion is presented as tagged user-role input rather than a synthetic
  tool pair. Prompt/context behavior changes intentionally, while manager output
  remains driven only by the model's subsequent response.

## Alternatives Considered

- **Repair the process-specific delivery ledger and retries.** Rejected because
  lifecycle policy was only one defect; the coordinator would still deliver
  outside the active ReAct loop and duplicate the inbox's queue/recovery role.
- **Append directly to `messages` and then ensure a runner.** Rejected because a
  producer can commit while an LLM request is in flight, placing new input before
  a response generated without seeing it. Reordering during transcript assembly
  is another hidden queue with weaker causal rules.
- **Keep synthetic assistant/tool completion pairs.** Rejected because these are
  independent inputs, not results of model-issued calls. User-role envelopes fit
  the existing boundary and cannot split a real tool call from its result.
- **Keep process delivery only in memory.** Rejected because a crash between
  process completion and the next loop boundary would silently lose the fact.
  The process lifecycle row and atomic inbox insert retain recovery without a
  second delivery ledger.
- **Route a late child-owned process directly to the root.** Rejected because a
  resumable child owns the process context and can synthesize a smaller, more
  useful follow-up. The child's next activation is asynchronous once any
  original foreground call has been resolved.
- **Kill every child-owned process when a child returns a final response.**
  Rejected because child sessions are resumable and final response ends one
  activation, not the session identity. A cooperative background-wait marker
  avoids an unnecessary intermediate completion when the model knows it is only
  waiting.
