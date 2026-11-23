# ADR-0103: A spawned session may name an effort level

- **Status:** Accepted
- **Date:** 2026-11-23
- **Amends:** [ADR-0100](0100-a-spawned-session-takes-a-tagged-model.md)

## Context

ADR-0100 let `spawn_session` name a tagged model but fixed the new root at that model's default effort,
leaving effort as a second parameter for later. An orchestrator asked to "start a reviewer on fast at high"
could not do it. The human had to `/switch` and `/model N high`, after the first turn had already run at
the default.

[ADR-0092](0092-task-effort-by-prompt-discipline.md) solved the same problem for subagents: optional
`task.effort`, set only when the user names a level, checked against the resolved model's catalog allowlist,
with a refusal that teaches the accepted levels. The daemon already has that check in one place, but its
refusals carried subagent wording.

## Decision

`spawn_session` takes an optional effort, set only when the user explicitly names a level. The daemon settles
the model first (ADR-0100), then checks the level against that model with the same check `task` uses. Both
happen before capacity, project lookup, worktree creation and the transaction, so a refusal leaves nothing
behind. Without a model, the level applies to the default model. Omitted, the root takes the model's default,
as before.

A level outside the model's allowlist is refused with the accepted levels. Any level for a model without an
effort selector is refused with "accepts no reasoning level; omit effort". The input is neither trimmed nor
case-folded. The accepted set is exactly what `/model` offers the human for that model. The human sees the
level with `/model` after `/switch`, as for a level set through `/model`. No tool changes an existing
session's level.

## Consequences

An orchestrator can start a root at a named level in one call. A wrong guess costs one refused call that lists
the levels. As with `task` (ADR-0092), nothing in code stops an agent from choosing a level on its own
initiative: effort changes cost and depth, never which provider receives the data. A higher level on a spawned
root costs more, and that root still has no budget unless the human sets one.

The `spawn_session` schema stays static and advertises no levels. The shared check's refusals no longer name a
subagent; `task` adds that wording at its own call site, so its texts are unchanged.

## Alternatives Considered

- **Advertising each model's levels in the schema.** This would put catalog facts into tool text that varies
  per configuration, as ADR-0092 rejected for `task`.
- **Silently dropping a level for a model without a selector.** Rejected by the owner: the orchestrator's model
  must learn that the model has no levels.
- **A spawn-specific validator.** This would duplicate the allowlist rule and its refusal texts.
- **Resolving the level inside the creation transaction.** That is where the default used to be resolved, but
  a refusal would come after the worktree and its project row exist.
- **An effort-switch tool or message.** Rejected by the owner: it changes a session the human controls.
- **Echoing the level in the acknowledgement.** Omitted calls must stay byte-identical, and success means
  exactly the named level.
