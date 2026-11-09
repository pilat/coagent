# ADR-0085: The core names no extensible tool by string literal

- **Status:** Accepted
- **Date:** 2026-11-09
- **Amends:** [ADR-0083](0083-core-recognises-tools-by-declared-traits.md)

## Context

ADR-0083 took `tool.X` identity checks out of the core and left string literals as
follow-up work: the tool-trail argument table in `session`; the category, description and
section tables in `sessionprompt`; the `mcp__` prefix; and the known search servers. Every
new tool with a trail argument, a prompt group or a prompt-section effect still had to edit
the core. The same follow-up list included `reg.Get("batch")` and registry allowlists;
they stay because `batch` is core-forever and allowlists are policy data.

The tables feed the system prompt, and the system prompt is part of the prompt-cache key,
so the move must not change a byte. A declaration on a live tool also cannot answer for a
call to a tool the session does not have, which the trail table could.

## Decision

An extensible tool declares what the core used to look up by name.

- **Trail argument:** `tool.TrailDeclarer` names the parameter and whether it is a path.
  The core keeps batch expansion, redaction, path shortening and clipping.
- **Prompt inventory and sections:** `tool.PromptDeclarer` returns one
  `tool.PromptDeclaration`: group, order within the group, description and the
  capabilities `Memory`, `Shell`, `WebFetch` and `Search`. The core keeps group titles and
  positions and all rendering. The untrusted-content section consults the existing
  `tool.UntrustedSource` instead of a new capability.
- **MCP search tools:** the tool stack passes `RegisterTools` a classifier that owns the
  known search servers and the `search` heuristic. Each MCP tool then declares `Search`
  like any other tool.
- **Core-forever tools stay named.** `sleep`, `schedule`, `task`, `send_to_subagent`,
  `get_subagent_result`, `batch`, `cancel_process`, `config_edit` and `config_read` are not
  extension points. Those listed in the inventory stay in the core's table, after any
  declared tools in their group. `config_edit` and `config_read` appear in no core table.
- **Enforcement:** a Semgrep rule forbids literals of the extensible tool IDs, the `mcp__`
  prefix and the search-server names in non-test code of `session`, `sessionprompt` and
  `sessionbuild`. Agent-type allowlists in `registry` are policy data and stay.

## Consequences

- A new tool in an existing group, or with a trail argument, does not edit the core. A new
  group still adds a row to the core's table, because the core owns titles and order.
- The rule's name list is closed. A test covers the tool stack, the memory tools and
  the owner tools named in the test. It fails until a new ID in that checked set is added
  to `.semgrep/coagent.yml`; adding a covered tool edits the rule, not the core. A tool
  added only through `registerSessionTools` or `ownerTools` must also be added to this test.
- Rendered tools sections are byte-identical. Goldens generated from the code before the
  move pin them, and each PR in the series left them untouched.
- One behaviour changes. A call to a tool the session does not have shows only its name in
  the trail: `→ websearch` instead of ``→ websearch: `query` ``. The call fails as an
  unknown tool anyway, and no prompt bytes depend on it.
- `internal/tool/builtin` and `internal/tool` grow by the declarations, and `sessionprompt`
  by a few lines because a scan replaces static tables.
- The rule matches whole literals, including multiline raw strings starting with `mcp__`.
  Prompt prose that names a tool stays, as ADR-0083 keeps it. Concatenation and conversions
  such as `[]byte("read")`, `[]rune("read")` and `interface{}("read")` escape the rule.
  An unrelated value that equals a tool name in these packages needs an inline
  `nosemgrep` with a reason.

## Alternatives Considered

- **Keep the argument for unregistered tools with a by-name table from the tool stack,**
  as the frame rule does. Rejected: a trail table would survive outside the
  tools, and every new tool would declare its argument twice, to protect the display of a
  call that is about to fail. The frame rule exists because stored rows outlive their
  tools; the trail is rendered only from a live response.
- **Search through `mcp.Profile`.** Rejected: profiles are keyed by exact server name and
  carry admission and gating, while the existing match is on the whole tool ID. A server
  named `exa__nested` counts as `exa`, and a profile would have changed those bytes.
- **Per-capability interfaces.** Rejected: one tool's prompt facts would spread over up to
  five methods with no second reader. One method and one struct keep them in one place.
- **A new capability for the untrusted-content section.** Rejected: on this code it would
  be true for exactly the tools that already declare `tool.UntrustedSource`.
- **Editing ADR-0083 in place.** Rejected: accepted ADRs are immutable, and this decision
  has its own tradeoff to record.
