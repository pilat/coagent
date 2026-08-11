# ADR-0014: A project subagent definition degrades; it never yields an unusable agent

- **Status:** Accepted
- **Date:** 2026-08-16

## Context

Project subagents come from `.md` files with YAML frontmatter in
`.claude/agents/` or `.coagent/agents/` — the same layout Claude Code and
several other agent runners use, so the files users drop in are usually copied
from elsewhere and were never written against coagent's schema.

Two frontmatter fields carried semantics that only surfaced at run time:

- **`tools:` omitted.** In the ecosystem those files come from, no `tools:` key
  means "inherit the full inventory". Here the absent key became a nil list,
  which `normalizeSubagent` turned into `["-todoread","-todowrite"]` — a
  non-empty list with no include directive, so `FilterTools` returned nothing.
  The type was still advertised in the prompt and still spawnable through the
  `task` enum; it just had zero tools, burned its iteration budget, and returned
  an apology. Nothing in the logs said why.
- **`model:` naming something the catalog cannot resolve** (`model: sonnet` — an
  alias, not a configured model id). The value flowed unvalidated into the spawn
  path and failed at `llm.NewClient` on *every* spawn, with an error naming the
  model rather than the file that set it.

Both are content read off a task repository, not user configuration. Coagent
fails hard on an unresolvable model in `config.yaml`, but applying that rule here
would let any checked-out repo break a session.

## Decision

A definition loaded from disk is interpreted as generously as it can be, and a
field the runtime cannot honour is dropped with one warning at load — never
carried forward to fail later, and never used to remove the agent.

- **An omitted `tools:` key inherits the full inventory** (`["*"]`), minus the
  subagent todo exclusions. An explicitly empty `tools: []` still means no tools:
  nil is "the author said nothing", `[]` is "the author asked for nothing". This
  lives in `registry.normalizeSubagent`, so every construction path shares it.
- **An unresolvable `model:` is dropped at the `subagentConfigs` seam**
  (`internal/session/setup.go`), logged once as `subagent_model_unknown` with the
  subagent name and its file path. The agent keeps its description, prompt and
  tools, and runs on the session's model. With no catalog configured there is
  nothing to validate against, so overrides pass through untouched.

## Consequences

- A copied agent file works on first spawn, which is the case that matters for a
  project users did not write for coagent.
- A subagent whose `model:` was meant to be a cheaper model silently runs on the
  session's model instead. That is a cost surprise, but a bounded one, and the
  warning names the file. Failing every spawn was the louder-but-useless
  alternative.
- `AgentTypeConfig.Tools` now distinguishes nil from empty. Anything constructing
  that struct programmatically and meaning "no tools" must pass `[]string{}`.
- Validation of loaded content belongs at the load seam. A new frontmatter field
  naming a runtime resource is expected to follow the same shape.

## Alternatives Considered

- **Skip a subagent whose model cannot be resolved.** Rejected: the parent still
  sees the definition file in the repo and its own prompt no longer lists the
  type, so the failure moves from "bad error message" to "silently missing
  capability" — the same class of problem as the toolless agent.
- **Fail session creation on a bad definition.** Rejected: session admission
  would then depend on arbitrary repository content, and a single stale agent
  file would make a project unusable.
- **Treat an omitted `tools:` as the read-only set.** Rejected: it invents a
  third meaning nobody writing the file intended, and a definition that describes
  writing work would silently be unable to do it.
