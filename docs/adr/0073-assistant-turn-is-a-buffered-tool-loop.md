# ADR-0073: The assistant turn is a buffered tool loop over a working-set desk

- **Status:** Superseded by [ADR-0074](0074-assistant-is-an-ordinary-session-with-a-desk-head.md)
- **Date:** 2026-10-19
- **Supersedes:** [ADR-0072](0072-assistant-desk-loop.md)

## Context

ADR-0072 made the assistant a stateless desk loop: every step re-rendered six note
scopes, a focus line, a budgeted project catalog, a hideable tape and one event, and the
model answered with one JSON object. Running it exposed three defects. A work session cut
from the catalog was unreachable (send, read and stop accepted only shown cards) and its
own `session.<id>` note silently disappeared with it; roots the user created were never in
the catalog at all. `forget` hid tape lines that nothing could find again. The desk had no
overall bound (40 lines of up to 6000 characters plus a 20000-character event).

Adding retrieval exposed the deeper tension. A model never decides to search for what it
does not know is missing (ADR-0072 already measured that an optional `recall` misses
unknown unknowns), yet forcing a search is impossible on current Claude models: Anthropic
returns 400 for `tool_choice` `any`/`tool` on Opus 5.5, Sonnet 5.5, Fable 5.1 and Mythos
5.1. Each extra desk re-render also re-pays the whole desk, and the model's intent is lost
between renders. The owner's view of the product settled it: a person works with a bounded
set of things in a given week; the desk should be that live working set, and older work
should be found on demand.

The constraint that survives from ADR-0072: the assistant is not a session, it is the only
voice in its dialog, and its context must be bounded without compaction.

## Decision

The assistant root still runs its own loop outside `internal/session`, but each inbox
input is one turn of native tool calls against one desk rendered once:

- The desk is a working set that states what it does not show: all live memory notes
  (small, permanent or temporary with an expiry date, per-note and total limits), the ten
  most recent focus items, the ten most recent assistant-touched sessions, the last ten
  conversation lines, and the event. Every truncated list names the tool that shows more.
- Look tools (`search`, `around`, `read`, `sessions`, `projects`, `focus_list`) run
  immediately; search is SQLite FTS5 over all of the manager's work roots, the tape and
  memory. Do tools (`open`, `send`, `stop`, memory and focus edits) are validated
  immediately but only recorded; they apply after one commit at the end of the turn, with
  handles standing in for sessions opened in the same turn.
- The model speaks in ordinary text. All text of the turn is buffered; several segments are
  consolidated into one reply, which is the only thing the user receives and the only
  thing that enters history.
- After the reply is fixed, a host guard checks bookkeeping by type (every open and send has
  a focus item referencing it; an answered or stopped session's focus item was updated;
  a search was written down; memory is within its limit) and asks the model to fix gaps;
  the guard never fails a turn.
- `tool_choice` stays `auto`; no response schema is sent.

## Consequences

- The desk is bounded by construction and small; old work is reachable by id after a
  search, so nothing is lost by falling off the desk.
- Intent survives within a turn because the turn is one conversation; across turns it
  survives only through memory and focus, which the guard enforces.
- Effects are atomic with the reply: a model failure mid-turn applies nothing. A crash
  after commit and before the effects run loses them (accepted for the POC; no replay).
- An old relevant fact is surfaced only if the model searches. The prompt pushes it to
  search whenever the user refers to something not on the desk.
- This re-runs the tool-call variant ADR-0072 rejected on measurement. The differences that
  are meant to fix it are untested by a harness: buffered effects, a single consolidated
  reply, and a guard that acts after the reply is fixed so it cannot shape its tone.
- The host no longer restricts which turns may write standing rules; a work answer can
  plant permanent memory.
- A turn costs more calls than one JSON step when the model looks things up; prompt
  caching covers the system prompt, tool definitions and desk across rounds on Anthropic.

## Alternatives Considered

- **Keep the JSON desk and add `## related` candidates chosen by host FTS, plus `search` as
  a continuing action.** Cheapest and closest to ADR-0072, but host search over the raw
  message cannot resolve references like "there" or "again", and every continuation still
  re-renders the whole desk and loses intent.
- **A separate librarian call or cheap model (Mercury) that digs before the desk and puts
  picked ids on it.** Kept the assistant tool-free, but added a second model and config, a
  persisted recall set that churned between turns, and a selector guessing what another
  model needs.
- **Two branches of one conversation (search, then roll back to the shared prefix and
  answer in JSON).** Changing `tool_choice` or the response format invalidates Anthropic's
  message cache, so the shared prefix did not save anything, and the tool suffix contradicts
  a JSON answer.
- **Forced first search with `tool_choice: required`.** Returns 400 on current Claude models.
- **Embeddings.** They fix morphology and language mixing but not anaphora or precision,
  need a separate provider (Anthropic has none), and a corpus of hundreds of sessions does
  not need them yet.
- **A `say` tool.** Text written outside it is lost, which ADR-0072 measured; ordinary text
  as speech removes that failure class.
- **A `done` tool or a "finished?" question to end the turn.** The turn ends when the model
  stops calling tools; the guard asks only about bookkeeping.
- **Durable effect intents with boot replay.** Deferred for the POC by the owner.
