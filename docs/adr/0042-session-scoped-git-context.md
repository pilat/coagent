# ADR-0042: Git context is a change-triggered delta on ingested input

- **Status:** Accepted
- **Date:** 2026-09-05

## Context

The model never learns which branch its session runs on or whether the checkout
already carries changes — it only knows the working directory from the
`# Environment` lines. It therefore guesses: it may assume a clean branch,
start editing on `main`, or burn tool calls on `git status` before even
understanding the task.

Three tensions shape the fix:

- **Cache stability versus freshness.** The conversation history dominates the
  request size, and provider prompt caches break at the first byte
  difference. Any volatile content placed anywhere in the system prompt —
  head or tail — invalidates the cached prefix for the entire history on
  every change. Claude Code accepts this cost; Codex and OpenCode avoid it by
  keeping Git state out of the model prompt entirely.
- **Unbounded versus bounded.** A diff or changed-file inventory has no size
  ceiling and duplicates what an explicit `git status` or `git diff` returns;
  commit subjects and remote URLs are user-controlled text that must not be
  fed to the model as trusted context.
- **Durable versus ephemeral.** Repository state is filesystem truth that goes
  stale the moment any command runs; persisting it as a separate durable
  structure (SQLite columns, controller DTOs) would freeze stale data into
  state other surfaces read.

## Decision

Git state is injected as a **change-triggered delta attached to ingested
input**, never into the system prompt:

1. When a durable input is accepted (user message in `drainBoundary`, direct
   run prompt in `prepareRunMessages`), the session probes its `WorkDir`
   through the read-only `internal/git` collector (branch, abbreviated HEAD,
   staged/unstaged/untracked/conflicted counts; five-second deadline; no
   paths, diffs, remotes, or stderr in results).
2. The probe's rendered report (~30 tokens) is compared against the last
   injected report, recovered by scanning the transcript tail for the
   `<git-state>` marker. Different, absent, or malformed → the report is
   appended to the input's prepared content and becomes durable history.
   Unchanged → nothing is appended.
3. Ambiguity always resolves toward sending: a redundant 30-token block costs
   less than the model acting on stale Git facts. Without a Git client
   nothing is injected at all.
4. Branch text is Go-quoted, capped at 128 runes by the collector; a failed
   probe sends `Git state: unavailable` rather than raw error text.

Because the delta rides on input bytes that are new to the request, the cached
prefix (system prompt plus the entire prior history) stays byte-identical in
every case. Within an activation no Git probe runs at all.

## Consequences

- The model starts every activation knowing the checkout state and learns of
  changes made between runs — including its own edits — at the next input,
  while paying zero incremental cache cost.
- Inputs are probed at ingestion; a probe adds milliseconds (bounded at five
  seconds worst case) to input handling, never to model turns.
- The transcript carries occasional `<git-state>` blocks in user rows; they
  are host-authored, fixed-format, and survive compaction inside the verbatim
  raw tail that the marker scan reads.
- Test doubles implementing `git.Client` must gain the read-only method.
- A mid-run file edit is not reported until the next input boundary; the
  model can still run Git tools for live state.

## Alternatives Considered

- **Bounded snapshot in the system prompt (this ADR's first version).** Lives
  after `# Environment`; simplest rendering, but any Git change between
  activations invalidates the cached prefix for the whole conversation —
  systematic cost, since the session's own edits dirty the tree. Rejected
  after cache-cost analysis.
- **Snapshot in the prompt tail (revised once).** Only isolates the damage to
  post-section content; the entire history still falls out of cache. Rejected.
- **Live refresh after every tool call.** Re-reads Git on every turn and
  invalidates caches constantly; the model already sees tool results. Rejected.
- **Diff or changed-file inventory.** Unbounded, duplicates an explicit tool
  call, feeds user-controlled paths into the prompt. Rejected in favor of
  four integer counters.
- **Durable Git-state columns.** Freezes stale filesystem truth into SQLite
  and leaks it into controller surfaces. Rejected — the marker scan reuses
  the transcript that already exists.
- **Commit subjects and remote identity.** Low task value, context cost,
  instruction-like user content. Rejected.
- **No automatic Git context (Codex/OpenCode path).** Zero cost, but the
  model guesses the checkout state or spends tool calls discovering it.
  Rejected for now; the delta design keeps the cost near zero anyway.
