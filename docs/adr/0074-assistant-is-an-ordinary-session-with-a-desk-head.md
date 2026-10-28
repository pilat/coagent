# ADR-0074: The assistant is an ordinary session whose activation starts from a desk

- **Status:** Accepted
- **Date:** 2026-10-28
- **Supersedes:** [ADR-0073](0073-assistant-turn-is-a-buffered-tool-loop.md)

## Context

ADR-0072 and ADR-0073 built the assistant as a loop of its own outside
`internal/session`, on the grounds that the session runtime could not give a bounded
context without a compaction policy and that the first attempt on that runtime (the
ledger coordinator) had failed for product reasons. The loop that resulted
(`internal/assistant`, about 3 800 non-test lines plus 800 of daemon, store and Telegram
glue) re-implemented what the session already owns — the model step and its finish
handling, tool dispatch, history, inbox drain, persistence and status — and added a turn
protocol of its own: do tools recorded and applied after one end-of-turn commit, handles
for sessions opened in the same turn, temporary ids for notes, a consolidation call for
several text pieces, a bookkeeping guard, and a tape instead of the `messages` table.
Every session-side mechanism keyed on root rows needed an exclusion for the type, and the
accepted crash window between commit and apply was debt the ordinary loop does not have.

The owner's judgment was that this is a superstructure, not a feature, and that the
`browser` subagent shows the shape the project wants: ADR-0067 rejected a separate
browser-use loop and ran the ordinary loop with a type-specific allowlist and a few
projection rules. The constraint from ADR-0072 that still holds is bounded context
without a compaction policy. The ordinary loop turned out to have the primitive for it:
`State.ResetContext` replaces a transcript with an opening turn inside one boundary
commit (the fresh schedule turn uses it), and a first user row is a compaction header that
is never summarized.

## Decision

The assistant is a root session of built-in agent type `assistant`, run by the ordinary
loop with no branch on its type inside `internal/session`.

- **The desk is the head of each activation.** `session.Input.Head` is a host function
  that renders a head string; the daemon builds it over the assistant store. At the first
  boundary of a run that accepts a model-bound input, with no pending tool call, the
  session prepends the rendered head as a user row and sets `ResetContext`, so every
  earlier row is compacted in the same commit and the model sees one desk followed by the
  inputs of this activation. Inputs that arrive mid-run append as in any session. The desk
  holds live memory, the newest focus items, the newest assistant-touched roots and the
  last forty conversation lines; the inputs are the event.
- **The assistant's tools are ordinary `tool.Tool`s with immediate effects**, registered
  as owner tools and named one by one in the type's allowlist, which removes the builtin
  stack. `open` returns the real session id; memory and focus edits write at once and
  enforce their own limits. No buffering, handles, temporary ids, end-of-turn commit,
  apply, consolidation or guard.
- **Reply semantics are the ordinary ones.** The first text with tools on a user input is
  a reply, the stop text is the final, the completion nudge is the second look, and the
  assistant always answers with text. Type properties on `registry.AgentTypeConfig` —
  `DisableIntegratedSearch` and `OmitFinalFooter` — replace string compares and keep the
  outbox content equal to the model's text.
- **Every hand-over carries the user's current message verbatim** with a fixed label
  warning that part of it may concern other work.
- **History is `messages`.** The tape and per-call records are dropped; search indexes the
  assistant root's own rows with an own-root arm; the desk's conversation lines come from
  accepted user inputs and the root's `message:`-keyed outbox rows.
- **`wakeWorthy` admits `agent` inputs for roots in status `error`** as it admits schedule
  inputs, matching boot's recoverable-input query, so a work answer wakes an assistant that
  failed its last run.
- Observation of work answers, the send ledger, topic binding, kill/clear refusals and the
  reconciler exclusion stay as they are.

## Consequences

- The context is bounded by construction: an activation starts from one desk, and the
  desk is the compaction header, so an in-turn auto-compaction can only summarize the
  turn's own tool traffic. Within an activation the model may look things up as many
  times as it needs.
- `internal/assistant` keeps only what is the assistant: store, desk render, tools,
  search, observation. The session loop gains one data hook and two type booleans; no
  other package learns the word "assistant" beyond the daemon and the manager.
- Effects are no longer atomic with the reply. A model failure after `open` leaves the
  session open and the user untold until the error notice and the next desk; a runtime
  failure of a tool-result commit re-executes the unresolved calls on the next activation
  (a second work root for `open`); this is the behavior of every session.
- An input accepted over unanswered work starts a fresh desk and compacts the unanswered
  rows; the desk shows the unanswered user line without a reply, and the prompt says it is
  still owed one.
- A user turn may produce two messages (a reply before tools, then the final); the prompt
  discourages text before tools. There are no silent turns: an event that changes nothing
  gets a short line.
- Each activation pays the ordinary build cost (`session.New`, the builtin stack with the
  hidden project's MCP servers, prompt build); accepted until measured.
- Roots of any type in `error` now wake on `agent` inputs. Today only assistant
  observations send those, so no other session changes behavior in practice.
- Two assistant properties recorded by ADR-0073 are gone: text written after the reply
  cannot be hidden from the user, and a search is not forced to be written down by the
  host. Both return to the prompt.
- ADR-0072's "the assistant is not a session" is withdrawn. Its measured failure of the
  tool-call variant belonged to a design with buffered effects and a separate reply
  channel; it is not re-measured here.

## Alternatives Considered

- **Keep the separate loop and trim it.** Rejected: the duplication is the loop itself,
  not its size; every fix to `internal/session` would need a twin.
- **Rebuild the desk on every accepted input.** Rejected: an input accepted between tool
  steps would discard the search results the model just received. Per activation keeps the
  turn's conversation whole.
- **Trigger the desk by size, as a compaction whose summary is host-rendered.** Rejected:
  the desk is the working set, not a summary of overflow; the model would carry earlier
  turns' tool traffic until a threshold, and the bound would be the threshold, not the
  construction.
- **Commit the head with `Replace` in a commit of its own before the boundary.** Rejected
  after review: `Replace` runs after boundary messages in `Commit`, forcing two commits and
  a crash window between head and inputs; `ResetContext` runs before them in the same
  transaction and already exists for the fresh schedule turn.
- **A `SilentStop` type flag** (an empty text stop as a terminal silent final) and **a
  sentinel reply filtered at delivery.** Rejected by the owner: a short line costs nothing;
  the flag is a loop exception and the sentinel lands in `messages`, outbox, desk and
  index.
- **Keep the final footer and store the clean text in the output's attributes.** Rejected
  once the owner dropped the footer for the type: one boolean beats a second copy of every
  final.
- **A per-type `runStatus` that never writes `error` for the assistant.** Rejected: more
  code to hide a real model failure; the wake rule makes `error` a label with a notice.
- **A type-specific completion nudge listing host-computed bookkeeping gaps, or gaps shown
  on the next desk.** Rejected for now: the guard answered a measured failure of the
  buffered variant; the generic nudge plus the prompt is the baseline until measured.
- **Fold focus and memory into the todo list or curated project memory.** Rejected:
  neither has expiry dates or session references.
