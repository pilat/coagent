# ADR-0069: A browser session takes one action per turn and gets a larger frame budget

- **Status:** Accepted
- **Date:** 2026-10-05
- **Amends:** [ADR-0067](0067-browser-subagent-supersedes-frames.md)

## Context

ADR-0067 gives a `browser` subagent one live **browser frame**: an earlier frame
is replaced once a `playwright` call from a later assistant message succeeds.
Frames from the same message stay live together, and every frame passes through
the ordinary 25 000-character insertion-time truncation.

The first runs exposed two problems. A Google results snapshot is about 50 000
characters, and head/tail truncation keeps the page header and footer while
cutting the organic results in the middle. The model got by on the screenshot.
In another step the model sent four `browser_navigate` calls in one message. It
wrote all four `task_state` notes before seeing any result, and four live frames
made a single 47.7k-token request. Every Playwright action changes the page and
returns a full frame, so a second action in the same message acts on references
the model has not seen.

Supersession already keeps history small: a live frame lives one step. A larger
frame therefore costs only the current request, unless one request carries many
frames.

## Decision

In a `browser` session, only the first `playwright` call of an assistant message
runs. Later `playwright` calls in that message are rejected by the host before
scheduling: the server never receives them, their rows are host-authored errors,
they never supersede the live frame, and they do not enter loop detection.

In a `browser` session, `playwright` results are truncated against a browser
frame budget of 120 000 characters, still capped by the context-window share
that bounds every tool result. When a frame is still cut, the omission marker
points the model at `browser_snapshot` with a selector, a visible element's
`ref`, or `depth`. Other sessions and tools keep the ordinary budget.

## Consequences

- One request carries at most one fresh frame, so the larger budget cannot
  multiply.
- Results in the middle of long pages reach the model in most cases. A page
  larger than the budget is still cut, but the model is told how to read the
  part that is missing.
- A step on a huge page costs more: up to roughly 30k tokens instead of 7k. The
  `len/4` token estimate is optimistic for snapshot YAML, so compaction triggers
  later on such frames.
- A multi-step interaction takes one model call per action. Forms are filled in
  one call with `browser_fill_form`.
- A sub-tree `browser_snapshot` succeeds, so it supersedes the full-page frame.
  Facts the model did not record in `task_state` are gone, as with any other
  action.
- Harmless read-only calls in the same message (such as listing tabs) are
  rejected too and are simply repeated next turn.

## Alternatives Considered

- **browser-use `multi_act`.** Up to five actions per step, and the chain stops
  after an error, after an action flagged `terminates_sequence`
  (navigate/search/go_back/switch), or after a URL or focus change. Rejected:
  browser-use captures state once per step, while Playwright MCP returns a frame
  per action. Copying it means superseding frames within a turn and parsing the
  page URL out of Playwright's text, and a `click` on a link cannot be flagged
  statically. This decision is browser-use with `max_actions_per_step = 1`.
- **One-action rule in the prompt only.** Rejected: it is not guaranteed, and the
  larger budget makes a violation expensive.
- **Structure-aware snapshot truncation** that keeps the top of the tree and
  collapses large sub-trees into `ref` stubs. Rejected: coagent would own a
  parser for a format Playwright controls and changes.
- **A larger budget for every session.** Rejected: roots and ordinary subagents
  keep their history, so large results would accumulate there.
