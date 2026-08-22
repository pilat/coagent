# ADR-0026: Tags bound autonomous subagent model choice

- **Status:** Accepted
- **Date:** 2026-08-21

## Context

Users may configure many models for direct selection through `/model`. Supplying
that entire catalog to the root agent also lets it autonomously choose any of
them for a subagent, even though catalog metadata cannot tell whether a model is
acceptable for a user's work. A daemon-wide `SUBAGENT_MODEL` fallback adds a
second implicit default and cannot express several acceptable candidates.

Model operations and session records identify configured models by model ID,
but unified configuration previously allowed the same ID under more than one
provider. Adding an ID-only tag mutation without resolving that ambiguity would
make policy depend on configuration order.

## Decision

Each configured model may carry a list of user-defined tags. A tag matches
`^[a-z0-9_-]+$`; its meaning is advisory and coagent defines no vocabulary,
ranking, or automatic routing. Catalog enrichment never creates or changes
tags. Configured model IDs are globally unique across providers, and duplicate
IDs make unified configuration invalid.

Humans continue to see every configured model in `/model`. The first configured
model remains the default, independently of whether it has tags. The `task` tool
advertises inheritance plus only models with at least one tag, showing their
exact IDs, catalog names, and tags without price, context, reasoning, effort, or
other catalog facts. An explicit `task.model` must exactly equal an advertised
tagged ID and is rejected before any child session or link is created otherwise.

Project subagent definitions remain human-authored repository policy under
ADR-0014: a configured definition override may use an untagged model, while an
unknown override degrades to inheritance. Child precedence is explicit tagged
task selection, then a valid definition override, then the parent model.
`SUBAGENT_MODEL` and its fallback branch are removed.

The reserved onboarding session receives `set_model_tags`, which replaces one
model's complete tag list through the existing restart-apply protocol. Exact
duplicates are removed with first-seen order preserved; an empty list removes
all tags. `add_model` does not invent tags. After adding a model, onboarding
explains direct `/model` availability and offers tag examples if the user wants
agents to consider it for subagents.

## Consequences

- The configured model list remains broad for humans while autonomous choice is
  a small, explicit user-controlled subset.
- An untagged default still works for every child through inheritance.
- Tags are flexible hints, not stable product roles; prompts may use them but
  code cannot assign semantics to `fast`, `review`, or any other value.
- Existing configurations with duplicate model IDs must be corrected before the
  daemon starts. This replaces already ambiguous first-match behavior.
- Removing `SUBAGENT_MODEL` is intentionally silent for leftover environment or
  secrets-file keys; they simply have no consumer.

## Alternatives Considered

- **Expose every configured model.** Rejected because configuration proves
  availability, not permission for autonomous selection.
- **Use fixed main/fast/scout/coding/review slots.** Rejected because roles
  overlap and create a second model configuration system.
- **Infer recommendations from catalog price, context, or reasoning metadata.**
  Rejected because those facts do not establish task quality or user acceptance.
- **Keep only a global subagent default.** Rejected because it hides policy in an
  environment knob and offers no bounded set of alternatives.
- **Make tags a fixed enum or route by them in code.** Rejected because
  user-defined guidance is more adaptable and avoids pretending tags are
  capabilities.
- **Allow duplicate IDs and qualify tag operations by provider.** Rejected
  because all current selection and persisted session identity is ID-based;
  partial qualification would leave the rest of the runtime ambiguous.
