# ADR-0088: The browser surface is the MCP server named in config

- **Status:** Accepted
- **Date:** 2026-11-10
- **Amends:** [ADR-0067](0067-browser-subagent-supersedes-frames.md), [ADR-0069](0069-browser-takes-one-action-per-turn.md)

## Context

ADR-0067 made the built-in `browser` subagent available when a session discovers
tools of an MCP server named exactly `playwright`, and deferred an explicit marker.
Its prompt and host notices name Playwright tools (`browser_fill_form`,
`browser_snapshot` with `target`/`depth`), and ADR-0069 rests on Playwright
returning a full frame after every action.

The owner wants to bind a different server: one that drives a whole computer
through screenshots, with its own tool set, where some calls (a status query)
return a short text instead of a frame. Under the current texts the model would be
steered to tools that do not exist.

We considered classifying frames by content (an image, or text above roughly
1 000 characters) so that short replies neither replace nor get replaced. It was
dropped: when a frame is replaced the model has already seen it and recorded its
facts in the mandatory `task_state`, so losing a short reply costs at most one
extra look. Classification by tool ID also stays deterministic for stored rows.

## Decision

The optional config section `browser.mcp_name` names, for the whole daemon, the
MCP server whose tools form the browser surface. The frame rule owns
`mcp__<mcp_name>__*`, and the `task_state` profile applies to that server. There
is no default: without the section there is no browser mode, and a server named
`playwright` is an ordinary MCP server. A present section that is null, empty, or
has a blank or untrimmed name is a configuration error.

Every result of the bound server is a frame. The ADR-0067/0069 mechanics are
unchanged: `task_state`, frame replacement in the projection, one action per
assistant message, the 120 000-character frame budget, and the `browser` type's
exclusive view of the surface.

Every browser-mode text the model sees is a tool-neutral discipline. It names no
Playwright tool and has no "browser vs computer" branches. The type keeps the name
`browser`. The child treats its assignment as the user's explicit request, and
returns any "ask the owner" need in its report. The root's browser guidance says to
start `browser` only when the user's request needs it, because the root never sees
the bound server's instructions ([ADR-0089](0089-mcp-instructions-follow-tool-visibility.md)).

## Consequences

- Any MCP server can be the browser surface, and the binding lives in one place.
- Configs that relied on the implicit `playwright` lose browser mode until they add
  the section. Their frames accumulate like any tool result again. Existing
  `browser` child sessions resume with no tools, because the empty rule owns none.
- Changing `mcp_name` reclassifies stored rows: an old browser session resumed
  after the change projects its old frames in full until compaction.
- With a server that answers actions with a short acknowledgement, the one-action
  rule costs two turns per action: act, then look.
- A server name containing `__` is still ambiguous as a prefix, as it was with
  `playwright`.
- One bound server per daemon: projects cannot bind different servers.

## Alternatives Considered

- **Keep `playwright` as the default.** Rejected by the owner: browser mode is opt-in.
  Its absence is not a regression.
- **Bind per project.** Declined: one browser server per daemon covers the use case.
- **Classify frames by content (image or text above a threshold).** Rejected for the
  reasons above. The model has already seen a replaced frame and recorded its facts
  in `task_state`, and a threshold adds a heuristic whose misses accumulate.
- **Infer frame-returning tools at subagent start** from names, descriptions or an
  LLM call. Rejected: MCP has no "returns an observation" annotation
  (`readOnlyHint`, `destructiveHint`, `idempotentHint`, `openWorldHint`, `title`
  only). Any guess must be persisted to keep projection stable, and descriptions
  need not say what a tool returns.
- **Describe server properties in config** for the configuring model to fill in.
  Rejected: that model cannot see the server's tools or results, so the values
  would be guesses that rot when the server changes. Runtime evidence (the tool
  schemas and the server's own instructions) is better.
- **Let the model toggle frame handling per call.** Rejected: another field the
  model can forget, and the omission is silent. ADR-0067 rejected prompt-only
  discipline for the same reason.
