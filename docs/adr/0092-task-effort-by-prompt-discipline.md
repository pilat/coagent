# ADR-0092: `task` carries an optional effort, governed by prompt discipline

- **Status:** Accepted
- **Date:** 2026-11-14

## Context

The `task` tool lets the parent choose a child's model ([ADR-0026](0026-tags-bound-autonomous-subagent-model-choice.md))
but not its reasoning effort. A user who says "run a reviewer on opus at high" gets a child at the parent's
inherited level, or at the child model's default, and nothing reports the mismatch. The spawn path already
has a slot for an explicit effort (`SpawnRequest.ReasoningLevel`). Today only a pinned `browser` fills it
([ADR-0091](0091-browser-runs-on-a-fixed-model-with-granted-built-ins.md)).

Model choice for subagents is bounded in code: only user-tagged models are candidates. Effort has no
equivalent user-curated subset. Each model's allowlist is catalog data ([ADR-0005](0005-per-model-effort-allowlist.md)),
and the allowlists differ widely. Advertising them in the `task` description would mean showing the catalog
facts ADR-0026 deliberately keeps out of it.

ADR-0091 rejected "effort without a model, applied to the inherited parent model", because an effort means
something only against a known model's allowlist. That reasoning is about **config time**, where no model is
known to check against. At spawn time the child model is always resolved first, and an explicit level is
checked strictly against that concrete model. So the objection does not carry over to `task.effort` with an
inherited model.

## Decision

`task` gains an optional `effort` parameter. Its use is bounded by prompt discipline, not by code: the tool's
description tells the model to set it only when the user explicitly names a level, and to omit it otherwise
so the child inherits.

- **Validation teaches.** An explicit level outside the resolved child model's allowlist fails the spawn
  before any child or link exists. The error names the levels that model accepts, and no levels are
  advertised up front. An explicit level for a model with no effort selector also fails ("accepts no
  reasoning level; omit effort") instead of being dropped silently. Inherited effort keeps its graceful
  fallback to the child model's default.
- **Precedence mirrors model precedence.** `task.effort` beats the agent type's configured effort. An agent
  type that pins its model (today `browser` with `browser.model`) ignores both `task.model` and
  `task.effort`.

## Consequences

- A user can name a child's effort through the agent in one sentence. A wrong guess costs one rejected call
  that tells the model the valid levels.
- Nothing in code stops the agent from choosing an effort on its own initiative. That is accepted: effort
  changes cost and depth, never which provider or model receives the data, so a prompt-level rule is
  proportionate.
- The `task` description and candidate list stay free of effort facts, so ADR-0026 is unchanged.
- The strict "no selector" error applies only on the spawn path. `/model` and session build keep the silent
  empty level for selector-less models.
- A pinned browser's effort can still be changed only through config.

## Alternatives Considered

- **`model#variant` in one string (opencode's shape).** Rejected: coagent carries effort as its own
  dimension in requests, records and `/model`, so a suffix would be a second encoding of one fact.
- **Advertise each tagged model's effort levels in `task`.** Rejected for now: it amends ADR-0026, and
  `inherit` has no single model to describe. It can be added later if rejected calls turn out to be common.
- **A code-enforced effort allowlist analogous to tags.** Rejected: there is no user-curated effort policy to
  enforce, and inventing one adds configuration for a parameter the user names in conversation anyway.
- **Let `task.effort` override a pinned browser.** Rejected: ADR-0091 pins the browser's model and effort
  together, and half a pin is not a pin.
