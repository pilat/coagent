# ADR-0077: The assistant moves the user into work, never out, and never speaks in commands

- **Status:** Superseded by [ADR-0082](0082-manager-is-a-groupless-chat-without-an-assistant.md)
- **Date:** 2026-10-29
- **Amends:** [ADR-0074](0074-assistant-is-an-ordinary-session-with-a-desk-head.md),
  [ADR-0076](0076-flat-chat-with-a-focus.md)
- **Amended by:** [ADR-0078](0078-assistant-input-is-never-human-authority.md) — reverses "Slash commands belong to the user" and "Budgets for work go through the user's /budget grant on #0"; the store barrier gives way to a human-input predicate
- **Amended by:** [ADR-0080](0080-assistant-administers-work-at-open-and-hears-direct-exchanges.md) — a model for new work is set in `open`, not by `set_model` afterwards

## Context

In the flat chat (ADR-0076) the assistant is the user's single voice, but it could not
bring the user into a work session or administer one. The user still had to type
`/switch N` and `/model N effort` themselves, and the assistant's `set_budget` reached only
the assistant's own root, a leftover of the management session.

Two facts from the code shaped the decision:

- The assistant does not know the chat. It runs in the daemon, while `chat_focus` is keyed
  by `(manager_id, chat_id)` and only the manager knows `chat_id`. The assistant also runs
  turns the user did not start, such as observer answers (ADR-0075) and its schedules.
- The assistant's instruction is enqueued as `source=user` with `from=assistant` and
  `observer_session_id`, and the quoting helper passed `/`-prefixed text through
  unchanged. A `send("/budget 5$")` or `send("/config …")` therefore created a durable
  ADR-0031 grant in the work session. That is the model-initiated authority ADR-0031
  forbids. The same channel reached `/compact`, `/status`, skill commands and the daemon's
  `/stop`, `/clear` and `/kill` dispatch.

`/budget` is not a manager command. On `#0` it is the user's grant for the assistant's own
`set_budget`, so budgeting work could not simply move to a tool without a grant.

## Decision

- **`switch` moves the chat focus into a work session, only from `#0`.** The manager
  creates its chat's `chat_focus` row (NULL) at startup without overwriting an existing
  focus. `switch` updates that row by `manager_id`, and only while it is NULL. It refuses
  when the chat is unbound, when the target is not a live work root the assistant may
  address, and when the user is already in a session (`the user is in session #N; they
  switch themselves`). Nothing moves the focus out of a work session except the user,
  whether to `#0` or to another session. `switch` writes no chat message; delivery treats
  it as an ordinary focus change.
- **Models are a manager path, not a grant.** `models` lists the configured models;
  `set_model` sets a work root's model and effort through the same daemon path as `/model`,
  on an ordinary request, never for the assistant itself.
- **Budgets for work go through the user's `/budget` grant on `#0`.** `set_budget` takes an
  optional session: omitted means the assistant's own root, a number means an addressable
  work root. The grant is checked and consumed on the assistant root, the target root is
  armed or cleared, and the receipt goes to the assistant's outbox only. `Arm` refuses a
  target that is parking after a fired checkpoint.
- **Slash commands belong to the user.** `send` and `open` refuse `/`-prefixed text. The
  store's `EnqueueTx` rejects any input with assistant provenance (`observer_session_id`
  or `from=assistant`) whose trimmed content starts with `/`, so no tool bug can reopen
  the channel.

## Consequences

- "Connect me to X" takes one assistant turn. The assistant cannot pull a user out of
  a session they are working in, including during turns the user did not start.
- The assistant's final after `switch` usually lands after the focus header and digest
  have gone out. It stays unread at `#0` until the user returns or the next digest.
- A chat becomes switchable only after its manager has started once on this version.
- ADR-0031 is unchanged. One `/budget` message still permits one successful mutation,
  alone in its response. A grant's root and the mutated root may now differ, and only the
  assistant uses that.
- An instruction that honestly starts with a path (`/etc/nginx.conf …`) is refused, and
  the prompt tells the assistant to begin with words.
- The work root's log gets no record of a budget or model the assistant set; `/status`
  in the session shows it.
- ADR-0074's "the assistant delegates project work" now also covers moving the user and
  administering work roots' model and budget. ADR-0076's "`/switch N` and `/assistant`
  move the focus" gains one assistant-initiated move: from `#0` into a session.

## Alternatives Considered

- **Let `switch` move the focus between any sessions.** Rejected: an observer answer or a
  schedule wakes the assistant while the user types elsewhere, and a "helpful" switch
  would send their next message to the wrong session.
- **Let the assistant move the user back to `#0`.** Rejected: leaving a session is the
  user's decision, and `/assistant` costs one word.
- **Pass `chat_id` to the daemon or keep a manager-to-chat table.** Rejected: one chat per
  manager already makes `manager_id` sufficient, and a startup insert-or-ignore needs no
  schema.
- **Budget work roots on an ordinary request, without a grant.** Rejected: it reopens
  ADR-0031 for the assistant, while a `/budget` message naming the session costs the user
  nothing extra.
- **A mandatory work-only `session` on `set_budget`.** Rejected: `/budget` on `#0` is the
  only way to budget the assistant itself, and it would be lost.
- **Refuse slash text only in the tools.** Rejected: the store is the trust boundary for
  ADR-0031 grants, and a tool-only check fails silently the next time someone adds a path
  that enqueues assistant text.
- **Write the budget receipt into the work root's log.** Rejected: it is the assistant's
  turn and `#0` must see it at once. The receipt key belongs to the assistant's tool call,
  and a second copy in the work log would be a duplicate the user meets later.
