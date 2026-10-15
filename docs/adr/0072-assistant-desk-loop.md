# ADR-0072: The assistant is a desk loop, not a session

- **Status:** Proposed
- **Date:** 2026-10-14

## Context

The product goal (one dialog, one assistant that remembers, routes and gathers work)
was first built as a coordinator session on top of the ordinary session runtime: a
work-item ledger (four tables), dispatch intents with quoted authorization, forwarded
user messages, work sessions publishing straight into the user's dialog, a bounded
projection of the coordinator transcript (the last five activations) and a rulebook
prompt of seventy never/always lines. It never merged; it is parked in a stash.

Three runs of a realistic dialog showed why. Two authors wrote under one name: a work
session's final answer went to the user unreviewed, so the dialog carried unverified
claims, machinery words and, when one question touched two projects, two contradicting
answers. The ledger asked the model for bookkeeping it did not care about
(acknowledgements, intents, quotes), and a weaker model ignored exactly those rules.
The transcript window dropped context after a handful of activations, and the
rulebook grew a patch per failure. Every fix bent `internal/session` further.

The constraint that survives: Coagent stays one daemon with one SQLite file, the
session runtime stays untouched for ordinary sessions, and the model's context must be
bounded without a compaction policy.

## Decision

The assistant runs as its own small ReAct loop (`internal/assistant`) over a **desk**
the host re-renders from the database on every turn: a keyed notebook, a few
stickers, one focus line, a read-only board of sessions, a numbered tape of recent
lines and the one new event. The model answers with exactly one JSON object holding
what to say, what to do with sessions (`open`, `send`, `read`, `stop`) and only the
edits to its slots: a written key replaces, a null key removes, an absent one stays.
A sticker may be pinned to a session. The host is the referee: it validates the answer
and the desk it leaves against fixed sizes and rules, re-asks on a violation, and
drops the step after the third. Slots
have fixed limits, so the prompt is bounded by construction and nothing is ever
compacted; the tape is host-kept and never deleted, only hidden on request.

The assistant is the only voice in its dialog. Work sessions are ordinary roots; their
answers to inputs the assistant sent come back to the assistant as events and are
retold by it. The assistant's durable identity is one root session row of agent type
`assistant`, so topic binding, inbound routing, outbox delivery and the wake machinery
are reused, but the session runtime never runs it.

## Consequences

- Each answer of a work session costs one extra model call and loses whatever the
  retelling drops; in exchange the user hears one voice and one answer per question.
- Dense direct dialog between the user and a work session goes through the assistant
  or happens in the session's own topic, where the assistant sees only the card.
- The prompt describes the world and the role; rules are habits with a reason. There
  is no tool-call mode, so the model is not pulled into terse executor behavior.
- The context is bounded without any compaction policy, trigger or backstop. Memory
  is never copied forward by the model, so an entry is lost only when the model
  removes it.
- Session-side mechanisms that key on root rows must exclude this type explicitly
  (progress reconciler, kill/clear, stop); a new agent type is a new case for them.
- Structured output is not enforced by the drivers; the referee is the enforcement,
  and a model that cannot keep to the JSON is unusable as the assistant.

## Alternatives Considered

- **Keep the ledger coordinator and fix its prompts.** Rejected: the failures were
  structural (two authors, bookkeeping the model ignores, a transcript window), and
  three rounds of prompt patches did not move them.
- **A long-lived chat session with compaction (threshold or model-decided) and a
  memory tool.** Rejected: every surveyed system (MemGPT/Letta, Claude Code, Codex,
  Anthropic compaction and context editing) still needs a host-owned overflow
  backstop, and the assistant would again be a session with special cases. The desk
  removes the growing transcript instead of managing it.
- **Rewriting the whole notebook, sticker list and focus every step** (the first
  version, run live). Replaced by edits: in a chained simulation (three scripted
  dialogs, three samples, blind grading) a rewrite dropped the user's standing rules
  from the notebook. It spent the most output tokens, and copying memory forward was
  about a quarter of a step's cost.
- **Native tool calls for the assistant's actions,** including a `say` tool and desk
  tools (`stick`, `note`, `focus`, …) applied together without tool results.
  Rejected on measurement. On the same simulation the tool-call assistant stayed
  silent on 9 of 20 handovers (JSON: 0 of 21) and left work unpinned on 10% of steps
  (JSON: 0%). Once it wrote its answer as plain text outside `say`. Host rules that
  reject these fixed the bookkeeping but scored lowest in blind grading. Knowledge was
  equal across forms. Letta reached the same point and dropped its `send_message` tool
  and tool rules.
- **Work sessions answering the user directly in the dialog** (ADR-0070 of the
  parked POC). Rejected: it is the root of the two-voices problem; it saved one model
  call per answer and cost coherence.
