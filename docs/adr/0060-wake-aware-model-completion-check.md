# ADR-0060: Model completion is wake-aware and requires a durable second look

- **Status:** Accepted
- **Date:** 2026-09-15
- **Supersedes:** [ADR-0053](0053-background-handoff-uses-ordinary-completion.md)

## Context

ADR-0053 removed the model-authored `<WAITING/>` marker. It correctly made an
ordinary no-tool `stop` sufficient to release a runner while a durable process
or background-subagent completion would later reactivate the session. The same
rule also treated every other no-tool `stop` as proof that the requested work
was complete.

Production use showed that this second inference is too strong. In long
unattended turns, a model can naturally end a response after narration even
though actionable work remains. Repeating completion instructions in the
system prompt is guidance, not a correctness boundary, and the provider finish
reason only describes why one generation ended. At the same time, requiring a
terminal tool or another exact text marker would recreate the model-facing
control protocol ADR-0053 removed.

The correction must apply uniformly to roots and subagents, preserve ordinary
background handoff, compose with todo state and one-shot budgets, retain the
manager reply obligation, and recover deterministically after daemon restart.

## Decision

The shared session loop distinguishes activation yield from task completion.
Response-integrity routing remains first: `length` and `unknown` keep ADR-0052's
durable rejection behavior, and actual tool calls continue structurally even
when a provider reports `stop`.

A normalized no-tool `stop` immediately completes the current activation when
the exact session has a durable background wake source: an advertised running
process, an undelivered non-blocking child in a state that promises automatic
delivery, or pending process/subagent inbox input. Producer ledgers are read
before the inbox so their atomic terminal-to-inbox transition cannot disappear
between observations. No nudge, waiting tool, or terminal tool call is required;
an empty response also yields on this path.

Without a wake source, a non-empty `stop` enters a durable two-phase completion
check. The first response is stored as an accepted assistant candidate and kept
in the provider transcript, but no manager output or subagent result exposes it.
The same transaction records its message ID on the session and appends one
host-authored user-role nudge. The nudge names only pending/in-progress todo
items when any exist; otherwise it does not mention todo. It asks the model to
continue with tools or respond once more with a concise explanation of why it
is stopping. The next accepted non-empty no-tool `stop` completes normally even
when todo remains open. No exact acknowledgement token is part of the protocol.

Any tool-bearing assistant response clears the candidate at response commit.
Any external model-visible input clears it in the transaction that inserts or
promotes that input; the typed host nudge is the sole exception. Candidate and
nudge stay pinned in the verbatim compaction tail until the check resolves.

Session rows retain the candidate message identity, an independent manager-reply
obligation, and the empty-stop streak. The manager obligation survives candidate
reset, tools, unrelated async input, and restart; only a releasing response or
terminal lifecycle output that supersedes the turn clears it. Process-local
fields are caches, never recovery authority.

An empty no-wake response is not confirmation. It enters the loop detector as a
distinct durable no-progress event, outside tool-diversity records. The third
consecutive empty response receives the established strong warning; the sixth
ends the activation successfully with one durable host notice. A root receives
that notice through its outbox. Subagent finalization derives the same successful
result from the durable terminal streak rather than fabricating an assistant
answer.

Each response disposition commits its assistant evidence, iteration, completion
state, empty streak, budget verdict, host nudge, and optional manager output as
one SQLite transition. Final output is rendered from post-disposition progress
facts inside that transaction through a pure renderer. A wake-projection failure
retains the paid attempt and usage as hidden evidence and, unless budget firing
wins, commits the existing durable error outcome instead of publishing or
forgetting the response.

## Consequences

- A model must deliberately stop twice when nothing else can wake it, while a
  model waiting on real background work still releases its runner immediately.
- Every normal no-wake completion spends one additional model call. Tool use or
  fresh external input invalidates the old confirmation and makes a later stop
  begin a new check.
- The first candidate is auditable and model-visible but never human-visible.
  Only the confirmed response releases the original manager input or becomes a
  subagent result.
- Daemon restart can recover the exact outstanding check, reply obligation, and
  empty streak. A crash before the disposition commit may still repeat the
  provider call because no durable response exists yet.
- Completion persistence, model-input ingress, compaction, final rendering,
  subagent finalization, budgets, and lifecycle outputs share new durable state;
  all must clear or preserve it according to this decision.
- Session lifecycle vocabulary is unchanged. Confirmed stop, background yield,
  and the sixth empty response use ordinary successful completion.

## Alternatives Considered

- **Trust every no-tool `stop`.** Rejected because provider-natural completion
  does not prove semantic task completion and caused unattended work to stop
  while actionable work remained.
- **Ignore `stop` and keep looping.** Rejected because background processes and
  subagents already provide a durable wake-up, and unconditional looping burns
  calls and recreates synthetic waiting behavior.
- **Require a final-answer or waiting tool.** Rejected because a model should not
  need a tool call merely to stand down, especially while a durable producer
  already owns the next wake.
- **Require `OK` or another exact marker.** Rejected for the same reason as
  `<WAITING/>`: exact assistant text is an unreliable second control protocol.
- **Keep confirmation state only in memory.** Rejected because restart between
  candidate and confirmation would duplicate the nudge or accept the candidate
  as final.
- **Make open todo an absolute terminal veto.** Rejected because todo can be
  stale or describe work the model cannot continue. It strengthens the second
  look, but the second explained stop remains authoritative.
- **Add incomplete/blocked session statuses.** Rejected because the existing
  lifecycle needs no new terminal vocabulary; the final explanation and todo
  retain semantic limitations.
- **Use a separate evaluator model.** Deferred because it adds model routing,
  cost, and failure semantics beyond the agreed same-agent second look.
