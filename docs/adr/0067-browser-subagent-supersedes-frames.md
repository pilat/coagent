# ADR-0067: A browser subagent supersedes earlier frames in its projection

- **Status:** Accepted
- **Date:** 2026-10-05

## Context

Browser automation through an MCP server returns an observation after almost
every action: page text and a screenshot. Two sessions driving a Playwright MCP
server showed what that costs today. `mcp.Client.CallTool` stringified each
`ImageContent` with `%v`, `truncateHeadTail` cut the base64 in half, and every
action added ~25k characters (~17k tokens). One shopping task reached 400k
prompt tokens in five minutes, and the model never saw a pixel. A root session
drove the browser itself, so the junk landed in the user-facing conversation.

Fixing transport alone (MCP images as ADR-0034 attachments) does not finish the
job. Browser observations are unlike ordinary evidence: each one describes a
mutable state, the next action makes it stale, and its coordinates become
harmful after a scroll. ADR-0034 rejected an image-eviction budget and left image
pressure to compaction, and the append-only context log keeps history immutable
between compactions. For a browser that means a full summarizer call every ~20
actions, condensing text that was fine just to drop stale pixels.

Peer browser agents converge on one shape. browser-use keeps only the current
screenshot and DOM per step and carries the past as model-written `memory`
fields, required by its output schema. Its flash mode reduces this to a single
`memory` field. A production browser agent (arXiv 2511.19477) puts the same
fields into each tool call's parameters. Anthropic's computer-use guidance keeps
the last three screenshots and prunes in batches to protect the prompt cache.

## Decision

When a session's tool registry contains tools of an MCP server named exactly
`playwright`, a built-in `browser` subagent type becomes available, and it alone
sees those tools. Every other agent type, project subagents included, has them
removed, and `browser` has nothing else. It runs the ordinary agent loop without
project context. A project subagent named `browser` is ignored.

Every `playwright` tool's schema gains a required `task_state` string, which
coagent strips before forwarding. It records the verdict on the previous action,
the task facts worth keeping from the current frame, and the next goal (the
browser-use flash `memory` contract). A call without a valid `task_state` is
rejected as a host-authored tool error, and the server is never called.

The whole result of a `playwright` call is a **browser frame**. In a `browser`
session's model-facing projection, built once in `messageStore.reloadMessagesLocked`, a
frame is replaced by one fixed placeholder (content replaced, attachments
dropped) as soon as a `playwright` call from a later assistant message has
succeeded. Stored rows, assistant messages, tool calls and reasoning or thinking
data are never changed. Every consumer reads the same projection: requests, the
compaction summarizer, token estimates and image pressure.

Independently of browser mode, MCP binary content never becomes text again.
Images become ADR-0034 attachments stored under the project's process-output
root, and other blobs become files referenced by path.

## Consequences

- A browser subagent sees its entire action trail and `task_state` notes, but
  only the newest frame or frames. Context grows by the call plus a placeholder
  per step instead of by a frame.
- Each step changes only the slot of the previous frame, which sits at the tail,
  so about one turn of prompt cache is lost per step. With one live frame,
  batching (needed for K>1) is unnecessary.
- Replacing a frame inside the last measured prompt prefix invalidates that
  provider token baseline in the same commit; sizing uses the current projected
  estimate until the next model response measures it again.
- This is the one exception to "no retroactive edits between compactions" and to
  ADR-0034's "no image-eviction budget". The exception is limited to
  `playwright` results inside the projection. ARCHITECTURE.md's statement that
  the summarizer sees full tool evidence no longer holds for superseded frames.
- A failed, blocked or skipped browser call never removes the live frame.
- A forgotten `task_state` costs one rejected call instead of silently losing a
  frame's facts. Facts the model did not write down are gone once the frame is
  superseded.
- Loop detection in browser sessions is driven by results: the result
  fingerprint includes an image digest, and the varying `task_state` keeps
  argument diversity high. Loops produce warnings, and escalation to a block
  rarely triggers.
- Browser mode depends on a server name, so a browser server registered under
  another name gets attachments but not the subagent. A `playwright` tool that
  already declares `task_state` is not exposed.
- The root is told to run one browser subagent at a time. This is not enforced,
  because persistent browser profiles reject concurrent clients.
- Attachment files accumulate without retention. Missing files degrade to the
  ADR-0034 placeholder, so cleanup can be added later.

## Alternatives Considered

- **Fix image transport only and rely on image-pressure compaction.** Rejected:
  it compacts the whole head every ~20 actions to drop stale pixels, and it
  summarizes text that never needed summarizing.
- **Keep the last three screenshots and prune in batches** (Anthropic,
  computer-use agents). Rejected: without a model-written note per step, three
  frames are needed for continuity, and each eviction reaches K turns deep into
  the cache. One live frame plus `task_state` is cheaper and loses nothing that
  was recorded.
- **A separate browser-use-style loop** that rebuilds system, state and text
  history every step. Rejected: it would reimplement persistence, resume, inbox,
  stop, ledgers and rounds, and it gives up prefix caching.
- **Prompt-only discipline** ("describe the frame, it will be removed").
  Rejected: an omission is undetectable and the loss is silent.
- **Field named `memory` or `state`.** Rejected: `memory` already names curated
  memory, and Playwright MCP's `browser_network_state_set` has a `state`
  parameter.
- **Supersede only pixels and keep frame text.** Rejected: page snapshots are as
  stale as screenshots and up to ~18k characters each. browser-use likewise
  keeps the DOM only for the current step.
- **Hide the tools only from the root.** Rejected: the root would delegate to
  `general` (`*` tools) and drown the same way.
- **Detect browser servers by explicit marker or by tool names.** Deferred by
  choice: the server name is the simplest rule today, and a marker can replace it
  without changing anything else.
