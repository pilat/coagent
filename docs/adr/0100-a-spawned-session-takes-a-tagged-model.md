# ADR-0100: A spawned session takes a tagged model

- **Status:** Accepted
- **Date:** 2026-11-19
- **Amends:** [ADR-0097](0097-orchestrator-mode-is-a-human-granted-capability-on-an-ordinary-root.md)

## Context

ADR-0097's spawn always started the root on the daemon's default model. An orchestrator delegating a cheap check
or a hard design task may need a different model. Allowing it to choose every configured model would bypass the
owner's tag policy, which already bounds the models an agent may explicitly select through `task`.

## Decision

`spawn_session` takes an optional `model`. The daemon validates it against exactly the tagged candidates `task`
offers, using the same candidate function. Omitted or empty, the new root starts on the daemon's default model.
The root takes the chosen model's default effort; the orchestrator cannot choose an effort level.

The daemon checks authority first, then the model before capacity, project lookup, worktree creation or the
transaction that creates the root and its watch. An invalid model is refused with the candidate IDs in config
order, or a statement that no tagged models are configured. The input is not trimmed.

Candidates reach the orchestrator through the `task` description it already receives. The `spawn_session` schema
stays static and points there rather than rendering a second catalogue. Successful acknowledgements, chat rows
and `list_sessions` stay unchanged. No tool changes an existing session's model; `/model` stays the human's.

## Consequences

- One tag policy governs autonomous explicit model selection through `task` and `spawn_session`.
- A spawned root still has no budget unless the human sets one. Tagging an expensive model now also lets an
  orchestrator run a whole independent root on it; spawn capacity limits concurrency, not spend.
- Explicitly naming an untagged default model is refused. Omitting `model` still selects it.
- A tagged model whose provider is misconfigured fails on the new session's first turn; no client is built at
  spawn time, and the ordinary error outcome signals the orchestrator.
- The shared candidate rule remains in `subagent`, beside its model information, rather than moving that type
  into a new neutral package. The daemon already depends on it; the orchestrator tool package stays model-free.

## Alternatives Considered

- **Every configured model, as in `/model`.** An agent could select a model the owner kept from autonomous use.
- **A model-switch tool or message.** Rejected by the owner: it would change an existing session the human controls.
- **Effort as a second parameter.** Outside this stage; the model's default level is the safe choice.
- **Rendering candidates into the `spawn_session` schema.** This would duplicate the catalogue rendering and thread
  model data into tool construction, which currently needs only the host and session ID.
