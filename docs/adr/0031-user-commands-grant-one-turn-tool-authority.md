# ADR-0031: User commands grant one-turn tool authority

- **Status:** Accepted
- **Date:** 2026-08-26

## Context

Some tools expose mutations that an agent may explain and prepare but must not
initiate from its own judgment. Hiding those tools until a command arrives makes
the live schema unstable and prevents a model already running in the session
from handling the command. Trusting a system-prompt note or matching command
text in the transcript is not an authorization boundary: models, schedules and
copied history can all produce the same text.

Queued inputs add another constraint. Several user messages may otherwise be
coalesced into one model turn, so authority intended for one slash-prefixed
request could accidentally cover a neighboring request or survive beyond the
turn that created it.

## Decision

Tools may declare exact `activation_commands`. A real manager-owned user input
whose first non-whitespace token matches one declared command is isolated by
the durable FIFO splitter and creates a durable grant bound to the exact input,
root session, command and tool. The user's message remains unchanged; the host
adds a deterministic model instruction naming the tool, but that instruction is
not authority.

The tool remains advertised at all times. Every gated mutation revalidates the
durable grant at execution. Invalid or diagnostic calls leave the grant
available, the first successful matching mutation consumes it atomically, and
every terminal path expires or cancels it. Later inbox inputs cannot be promoted
while the grant is unresolved. An assistant response that combines the gated
mutation with any sibling tool call is rejected before side effects.

The final live registry must have at most one tool declaring an exact command;
duplicates are a session setup error. Initially only `set_budget` declares
`/budget`; existing `/status` and `/skill` handling does not migrate to this
protocol.

## Consequences

- Models can see and discuss gated tools without acquiring mutation authority.
- Authorization survives compaction and restart because it is durable user
  provenance rather than prompt text.
- A slash-prefixed user input is a FIFO turn barrier even when no activation
  command matches it.
- Every runner exit must resolve a pending grant or later input would deadlock.
- Commands are exact, case-sensitive tokens; lookalikes such as `/budgetx` do
  not match.
- One activated command turn permits at most one successful gated mutation and
  cannot mix that mutation with unrelated effects.

## Alternatives Considered

- **Expose a `/budget` parser in the harness.** Rejected because free-form
  interpretation belongs to the model and would create a second command
  language in the daemon.
- **Dynamically add the tool only after the command.** Rejected because it makes
  tool discovery activation-dependent and complicates an already-running
  session's registry.
- **Authorize from prompt text or the augmented message.** Rejected because
  model-authored, scheduled or replayed text could forge the same string.
- **Consume authority on the first call, even an invalid one.** Rejected because
  the model must be able to correct typed arguments within the user-authorized
  turn without asking the user to repeat the command.
- **Let authority cover a coalesced batch of user inputs.** Rejected because the
  scope would no longer be the turn in which the user explicitly delegated it.
