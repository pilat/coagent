# Testing Strategy

What evidence a change needs. How to spell an assertion is a style question and
lives in [coding-style.md](coding-style.md).

Coagent is durable and asynchronous. The failures that matter do not live inside
one function — they show up only after a sequence like enqueue, yield, complete,
restart, redeliver. A large unit suite can be entirely green while the first real
conversation is wrong.

## When this document applies

Read it before designing tests for a change that touches:

- session, subagent, schedule or manager lifecycle;
- durable state transitions, queues, ledgers, migrations, replay or recovery;
- goroutines, cancellation, retries, deduplication or event ordering;
- more than one runtime package on a request or notification path;
- output a user actually sees.

For those changes, unit tests are not sufficient on their own.

## Levels

**Unit.** One function or one package-local state transition. The right place
for parsing, validation, formatting and narrow error paths. Proves nothing about
ordering across runners, persistence or delivery.

**Model-based protocol tests.** For stateful protocols whose correctness depends
on order, retries, duplicates or restart. A small reference model defines the
legal states; a driver applies the same command sequence to the model and to the
real adapter and compares observable state. Deterministic traces first, fuzzing
after.

Invariants worth stating this way:

- every accepted input is pending, handled, rejected or cancelled — never lost;
- each child activation produces at most one parent completion;
- an event from an older activation cannot complete a newer one;
- unresolved tool calls stay paired and are never silently crossed.

**Scenario integration tests.** Real store, real session, real event flow, a
scripted model. They assert the conversation a user would end up seeing, not
intermediate calls.

## Rules

- Every bug reported from a real conversation becomes a deterministic fixture
  first, preserving the event order and the exact observable symptom. Then it
  gets fixed.
- A scripted model is allowed to misbehave. Prompt compliance is never a
  correctness boundary.
- Tests are hermetic with respect to user state: isolate `HOME` under
  `t.TempDir()` before resolving any coagent-home path.
- Say "E2E" only for tests that drive a compiled process.
