# ADR-0083: The core recognises tools by declared traits, not by name

- **Status:** Accepted
- **Date:** 2026-11-08
- **Amends:** [ADR-0064](0064-untrusted-tool-output-is-data.md)
- **Amended by:** [ADR-0085](0085-core-names-no-extensible-tool-by-literal.md) — the extensible-tool literals this ADR left in the core moved to tool declarations, and a trail line for an unregistered tool shows only its name

## Context

The import direction between packages is guarded by `.go-arch-lint.yml`. Coupling
by name was not guarded. The core (`session`, `sessionbuild`, `sessionprompt`,
`registry`, `subagent`, `mcp`) tested tool identities exported by `internal/tool`
and changed behaviour on them: `IsBrowserToolID` and `BrowserAgentType` for the
browser frame rules of ADR-0067 and ADR-0069, `PlaywrightServerName` for the
`task_state` argument, `IsUntrustedOutputSource` for ADR-0064's wrapping of direct
errors, `IDSkill` for the skills inventory and the skill envelope kept across
compaction. On 2026-11-07 there were 48 such references in 15 files. Every new tool
with a special property had to teach the core its name.

A trait declared by a live tool is the obvious replacement, but it cannot answer
for a tool that is not registered right now. Two cases matter. A stored row whose
tool has gone away: a browser subagent resumed after its Playwright server was
disabled, or after an upgrade renamed a tool, still has frames of up to 120 000
characters in its transcript. And a call to a tool that does not exist, which
ADR-0064 deliberately wrapped as untrusted because its classifier saw only the
tool ID.

Some control tools are part of the core for good: agents, tasks, subagents,
background processes and jobs, with their tools `sleep`, `schedule`, `task`,
`send_to_subagent`, `batch`, `cancel_process`, `config_edit` and `config_read`, and
`IsExternalCall`. They are not extension points, and naming them is not coupling.

## Decision

The core asks a tool what it is like, never what it is called.

- **Traits on the tool.** A property of a live call is an optional interface on
  `tool.Tool`, declared by the tool and consulted by the core:
  `tool.UntrustedSource` (direct errors are untrusted output; `webfetch`,
  `websearch`, every MCP tool) and `tool.SkillLoader` (enables the skills
  inventory; its rows may carry the current skill across compaction).
- **A frame rule from the tool stack.** Browser frames are classified by
  `tool.FrameRule`, which the owner of the browser tool surface declares by tool
  ID and the tool stack hands to session assembly regardless of which servers are
  up. The session applies ADR-0067 and ADR-0069 through it, unchanged. An agent
  type opts in with `AgentTypeConfig.FrameSession`; such a type sees only the
  rule's tools and every other type never sees them.
- **The browser tool surface is owned by `internal/tool/builtin`**: the
  `playwright` server name, the tool-ID prefix, the 120 000-character frame
  budget, and the `task_state` schema, validation and stripping. `internal/mcp`
  knows no server names; it applies a per-server `mcp.Profile` supplied by the
  tool stack.
- **Agent-type names belong to `registry`**, which also answers which names
  project subagents may not take (`registry.Reserved`).
- **Who may name a tool.** A tool's own package may name it: `internal/tool/**`
  (including `builtin` for `skill`) and `internal/mcpstore` for `mcp_add`,
  `mcp_remove`, `mcp_enable`, `mcp_disable` and `mcp_list`. The core-forever tools
  above may be named anywhere. `cmd/coagent` is the composition root and is
  exempt. Two Semgrep rules in `.semgrep/coagent.yml` forbid any other
  `tool.ID…`, `tool.Is…`, `tool.Browser…` or `tool.Playwright…` reference
  outside tests.
- **Amends ADR-0064.** Untrusted classification of a direct error now asks the
  tool instance. A call whose tool is not registered has produced no external
  content, so its host-written "unknown tool" error is no longer wrapped.
  Registered web, search and MCP tools, including their panics and host-side
  rejections, are wrapped exactly as before.

## Consequences

- A new tool with a special property declares a trait; the core does not change.
  A new tool family whose results must be classified after the tool is gone
  declares an ID rule through the tool stack, as the browser does.
- Browser frame handling, untrusted wrapping of registered tools, tool schemas
  and prompt assembly are byte-identical to before.
- Two behaviours change for tools that are not registered: their "unknown tool"
  errors lose the untrusted wrapper, and a skill envelope on a stored `skill` row
  is carried across compaction only while the skill tool is registered.
- The rule sees `tool.X` selectors only. String literals still name tools in the
  core: the tool-trail argument table in `session`, the category, description and
  section tables in `sessionprompt` (including the `mcp__` prefix and the known
  search servers), `reg.Get("batch")` in session assembly and the agent-type
  allowlists in `registry`. Moving them to tool declarations is follow-up work.
- Browser notice and prompt text (the frame placeholder, the extra-action notice,
  the cut hint, the task tool's browser guidance, the browser agent prompt) stays
  where it is; only identity checks moved.

## Alternatives Considered

- **Traits on the live tool only.** Rejected for frames: a browser subagent resumed
  without its Playwright server would re-send every old frame instead of the
  placeholder. Accepted for untrusted output and skills, where the drift is
  limited to tools that do not exist.
- **ID rules for every trait.** Byte-identical even for nonexistent tools, but
  every trait would need a second way to be declared, and ADR-0064's over-marking
  of unknown-tool errors protects no external content.
- **A wrapper tool for Playwright tools in `builtin`.** Rejected: the wrapper would
  have to forward every optional interface (`UntrustedSource`) and would reorder
  the empty-schema check ahead of the `task_state` extension.
- **A new `internal/browser` package.** Rejected: `builtin` already builds the
  stack, registers MCP tools and is exempt as part of `internal/tool/**`; a new
  component adds dependency-rule and package-map churn for nothing.
- **Move the string-literal tables in the same series.** Deferred by the owner: it
  touches every built-in tool and prompt text the prompt cache depends on, and the
  rule could not enforce it.
