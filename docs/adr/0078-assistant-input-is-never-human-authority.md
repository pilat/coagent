# ADR-0078: Assistant input is never human authority

- **Status:** Accepted
- **Date:** 2026-10-30
- **Amends:** [ADR-0031](0031-user-commands-grant-one-turn-tool-authority.md),
  [ADR-0077](0077-assistant-moves-the-user-in-never-out.md)
- **Amended by:** [ADR-0080](0080-assistant-administers-work-at-open-and-hears-direct-exchanges.md) — human input is `source = 'user'` without `from = assistant`; `observer_session_id` alone no longer removes authority
- **Amended by:** [ADR-0081](0081-assistant-forwards-and-the-host-relays-work-answers.md) — a forwarded user message is human input, but never lifts a budget and never carries a slash command

## Context

ADR-0077 closed a real hole: the assistant's instruction is enqueued as
`source=user`, so a `send("/budget 5$")` or `send("/config …")` created an ADR-0031
grant in a work session. It closed the hole bluntly. The store and `send`/`open`
refused every `/`-prefixed assistant text, so the assistant could no longer run a
session's skills or commands, or even start an instruction with a path. Budgeting a
work session needed a separate `/budget` message from the user on `#0` for every
change, although the user had just asked for it in plain words.

The code showed where authority actually follows from a user input:
- tool activation grants, built in the session boundary (`acceptBoundaryText`) and
  inserted by `insertActivation`;
- lifting a fired budget, which any user input to a parked root did
  (`prepareManagerModelInputTx`).

Everything else keyed on `source = 'user'` is mechanics: command dispatch, resume and
wake eligibility, skill receipts, search and cards.

Two facts constrained the fix. First, if assistant input simply stopped lifting a fired
budget, the parked session would accept the text, spend a model call, refuse to execute
its tool calls and park again. Second, the session layer treats any `set_budget` direct
receipt in a `/budget` turn as spending the grant (`consumeActivation`), so a grant-free
call inside that turn would strand a durable `pending` grant.

## Decision

- **Human input** is `source = 'user'` with no assistant provenance
  (`observer_session_id`, `from=assistant`), decided by one `sessionstore` predicate.
  Only human input creates activation grants (in the session boundary and in
  `insertActivation`) and lifts a fired budget.
- The slash barrier is removed. The assistant may send any text, including commands
  and `/skill` lines, and they behave as the user's. `/stop`, `/clear`, `/kill` and
  `/status` from the assistant are dispatched like the user's, after its delivery
  releases the tree lock.
- Assistant input to a work root whose budget has fired is refused at the store
  boundary: `session #N is parked at its budget checkpoint; raise or clear its budget
  with set_budget first`. Lifting a checkpoint is always an explicit `set_budget`
  with a receipt in `#0`.
- The assistant arms and clears its work roots' budgets on an ordinary request, without
  a grant (a delegated mutation). The store accepts a grant-free mutation only when the
  receipt root is an assistant root and the target is a different, live,
  non-assistant, owned root of the same manager. A replay is recognized by the
  existing receipt key `tool:<call>:direct:0`.
- The assistant's own budget still requires the user's `/budget` on `#0`. Within a
  `/budget` turn, the first successful `set_budget` spends the grant whatever its
  target, and the "alone in its response" rule stays generic, keyed on the tool name.
- `config_edit` and `config_read` exist only on the assistant root. `/config` typed in
  a work session is refused by the manager: `/config works on the assistant:
  /assistant, then /config`.

## Consequences

- The assistant can run session commands and `/skill <name>`, and can start an
  instruction with a path. Assistant `/kill` and `/clear` on its work are as powerful as
  the user's, by design.
- Sites that keep plain `source = 'user'` checks because they carry no authority:
  - generic command dispatch and boot/wake resume eligibility;
  - inbox resume SQL;
  - control-command blocking at the boundary;
  - skill receipts and the `preserveStopped` reset;
  - `ActiveUserInput`, assistant search indexing, card fallback and conversation
    reads;
  - session-title naming;
  - `CreateManagerRoot`'s prompt.
- The assistant's `stop` tool enqueues `/stop` without provenance and therefore counts
  as human input; `/stop` grants nothing, so that is harmless.
- A `/budget` turn that budgets the assistant and a work session needs the own change
  first; the work change after it is delegated.
- Work roots lose the config tools. A config apply staged on a work root by an older
  binary and answered by this one is not handled.
- An assistant `/skill <unknown>` is rejected in the work log, and the assistant learns
  of it only by reading.

## Alternatives Considered

- **Keep ADR-0077's barrier.** Rejected: it forbids legitimate commands and skills to
  close a hole that exists only where input turns into authority.
- **Let assistant input lift a fired budget like the user's.** Rejected: the limit
  would vanish as a side effect of "continue", with no receipt.
- **Let assistant input into a parked root through without lifting.** Rejected: it
  spends a model call and loses the instruction.
- **Authorize grant-free budget mutations only in the tool.** Rejected for the same
  reason ADR-0077 put the barrier in the store: the next caller of `Arm` would mutate
  any root, the assistant's own included.
- **A target-aware "alone in its response" rule, or a tool signal that a grant went
  unused.** Rejected: both extend the activation contract between `session` and `tool`
  for a rare multi-session `/budget` turn.
- **Refuse `/stop`, `/clear`, `/kill`, `/status` in `send`.** Rejected: it narrows "the
  assistant sends any command" when dispatching them like the user's costs one branch.
