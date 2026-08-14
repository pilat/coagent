# Testing Strategy

This document defines **what evidence a change needs**, not how to spell an
assertion. Go test style, mocking conventions, and test libraries live in
[coding-style.md](coding-style.md).

Coagent is a durable, asynchronous system. Many important failures do not live
inside one function: they appear only after a sequence such as enqueue → yield →
complete → restart → redeliver → render. A large unit-test suite can therefore be
green while the first real conversation is wrong. Tests must cover the protocol
over time and the output visible to a controller, not only individual methods.

## Mandatory trigger

Before designing tests, read this document when a change touches any of:

- session, subagent, schedule, manager, or controller lifecycle;
- durable state transitions, queues, ledgers, migrations, replay, or recovery;
- goroutines, admission, cancellation, retries, deduplication, or event ordering;
- more than one runtime package on a request or notification path;
- user-visible CLI, Telegram, or other controller output derived from events;
- tool protocol: suspension, asynchronous completion, follow-up, or wake-up.

For such a change, unit tests alone are not sufficient. Select the additional
level from the evidence matrix below and state the invariant the test protects.

## Test levels

### 1. Unit tests

One function, type, or package-local state transition in isolation. These are the
fastest proof of local behavior and the right place for parsing, validation,
formatting, and narrow error paths.

Unit tests do **not** prove ordering across runners, persistence, PubSub, or a
manager renderer. Mocking those boundaries turns the behavior under test into an
assumption.

### 2. Model-based protocol tests

Use for a stateful protocol whose correctness depends on event order, retries,
duplicates, races, or restart. A small reference model defines legal states and
invariants independently of production implementation. A test driver applies the
same command sequence to the model and to the real protocol adapter, then compares
observable state. Deterministic traces come first; fuzzing generates and shrinks
additional traces.

Typical commands:

```text
user_message  spawn_foreground  spawn_background  send_followup
child_complete  duplicate_completion  stop  kill  crash  restart  timer_fire
```

Typical invariants:

- every accepted input is pending, handled, rejected, or cancelled — never lost;
- each child activation produces at most one parent completion;
- an event from an older activation cannot mutate or complete a newer one;
- unresolved tool calls remain paired and are never silently crossed;
- stop does not resume work; restart preserves the same observable obligations;
- historical output is state, not a new publication event.

Naming and placement:

- `harness_model_test.go`
- `TestHarnessModel_<Invariant>` for named deterministic traces
- `FuzzHarnessProtocol` for generated command sequences
- minimized fuzz failures remain in the Go fuzz corpus

A reference model tested only against itself is not evidence. It must be compared
with production behavior through a narrow adapter, or be used as the oracle for
the same generated trace.

### 3. Scenario integration tests

Use for a concrete workflow crossing production components or producing
controller-visible output. Run the real daemon/session loop, temporary migrated
SQLite, durable inbox/ledgers, PubSub, and real manager rendering. Replace only
irreducibly external boundaries: use a scripted LLM and a fake Telegram HTTP
transport (or an in-memory controller sink).

Assert the complete observable trace:

- messages and events appear in causal order;
- each user-visible result appears exactly once;
- no internal sleep/resume/polling chatter leaks into the conversation;
- stored transcript, durable ledger, and rendered output agree;
- restarting from the same temporary database does not add output or lose work.

Naming and placement:

- `harness_scenario_test.go`
- `TestHarnessScenario_<Workflow>`
- reusable traces/golden conversations under `internal/testdata/harness_scenarios/`:
  the daemon scenarios *record* the ordered controller trace there (regenerate
  with `go test ./internal/daemon -run TestHarnessScenario -update-traces`), and
  the telegram harness *consumes* the same files, replaying them through the
  production renderer against its own rendered golden (regenerate second:
  `go test ./internal/managers/telegram -run TestHarnessScenario -update-traces`).
  A new daemon scenario that records a trace therefore also requires
  regenerating the telegram golden — deliberate coupling, per the compositional
  rule above

When package boundaries deliberately prevent one test package from constructing
both the daemon and a concrete manager, prove the boundary compositionally:

1. the daemon scenario asserts the exact `SessionNotification` trace reaching a
   controller sink;
2. the manager scenario feeds that trace through the production renderer and
   asserts the exact external transport requests.

Neither half may replace the other. PubSub-only evidence cannot prove Telegram
rendering, and renderer-only evidence cannot prove that the daemon emits each
notification once.

These tests are hermetic and belong in the default `go test ./...` suite. Do not
add an `integration` build tag merely because several internal packages are used.
The tag is reserved for tests requiring installed programs, containers, or other
environment prerequisites. Canonical test targets do not call real network
services; use local servers and temporary local Git repositories.

### 4. End-to-end smoke tests

Reserve “E2E” for the compiled binary and its real process boundaries: control
socket, daemon process, migrated database, and fake external servers. E2E proves
wiring and packaging, not the combinatorial protocol state space. Keep it small.

## Evidence matrix

| Change | Required evidence |
|---|---|
| Pure computation, parser, formatter, validation | Unit |
| One store operation or migration | Unit with real temporary SQLite; fresh and prior-schema migration paths |
| Lifecycle, queue, ledger, retry, dedup, restart, or concurrency semantics | Unit + model-based protocol |
| Cross-package user workflow | Unit + scenario integration |
| Controller-visible output or event rendering | Scenario integration asserting the final conversation |
| Temporal protocol that is also user-visible | Unit + model-based protocol + scenario integration |
| Compiled-process/control-socket wiring | E2E smoke |
| External tool/container/network behavior | Tagged environment integration test |

When uncertain, classify by the failure being prevented. If the failure can be
described only as “after A, while B, then C”, it is temporal and needs more than a
unit test.

## Orthogonal test amplifiers

These strengthen an evidence level; they do not replace one:

- Run the race detector when the changed path uses goroutines, channels, locks,
  concurrent database writers, or asynchronous completion.
- Fuzz parsers and model-based protocols whose useful input space is larger than
  a reviewable table. Commit every minimized failure corpus. A fuzz failure in
  the reference model is still a real test defect and must be understood rather
  than deleted.
- Mutation-test new load-bearing guards, retry/dedup conditions, and error paths.
  Scope mutations to the changed production area. Report killed, lived, and
  uncovered mutants separately; a run that generated zero mutants is not a pass.

Coverage answers whether code ran. Mutation answers whether the assertions would
notice relevant code becoming wrong. Neither proves a cross-package observable
workflow; that remains the job of scenario or E2E evidence.

## Local CI gate

This repository currently has no hosted CI. `make ci` is the canonical slow
local pre-merge gate. It composes `make all`, compiled-process harness E2E,
build-tagged environment integration, long-running protocol fuzzing, the full
race detector, shuffled repeated protocol scenarios, and thresholded mutation
testing. The Make target is the source of truth for budgets and exact commands;
this document defines why those checks exist.

`make check` adds build-tagged environment integration against locally installed
programs such as `git` and `gopls`. Git repositories are temporary local fixtures:
the suite must never clone a mutable network repository. `make ci` includes this
tagged suite as well as repeated E2E, fuzz, race, stress, and mutation evidence.

Budget overrides are for iteration only. A shortened fuzz duration, stress
count, or E2E count must be reported as a smoke run, never as a passing canonical
CI run. The final gate uses the Makefile defaults.

## Hermetic filesystem and network boundary

Tests must never read or write the invoking user's coagent home. Any test that
resolves a home-level coagent path must set `HOME` to `t.TempDir()` or call
`coagenthome.Override` with a temporary directory before resolution. Test
binaries fail closed when `coagenthome.UserHome` would return the inherited home,
an alias of it, or a path beneath it. Direct `os.UserHomeDir()` calls are banned
outside `internal/coagenthome`, including in tests, so this guard cannot be
bypassed accidentally.

Compiled-process tests must replace, not append, inherited `HOME`, `USERPROFILE`,
and every `XDG_*_HOME` directory hint. Use the shared isolated subprocess
environment helper and keep all replacements beneath the test-owned home;
duplicate `HOME` entries and an isolated `HOME` paired with a real XDG path are
both boundary violations. The `coagent-no-inherited-home-in-process-tests`
semgrep rule prevents compiled daemon tests from rebuilding an environment by
appending values directly to `os.Environ()`.

Tests must also not name or clone a real marketplace repository. Exercise parsing
with synthetic names, loader behavior with temporary directory fixtures, and Git
clone/pull behavior with an initialized repository under `t.TempDir()`. The
`coagent-no-real-marketplace-fixtures` semgrep rule protects the known regression;
the general rule remains that mutable upstream content is never a test oracle.

## Bug-fix workflow

1. Preserve the reported sequence as a failing deterministic scenario before
   changing production code. Assert the exact user-visible symptom.
2. Name the violated invariant. “This line is wrong” is not an invariant;
   “one completion per child activation” is.
3. If the bug represents a class of reorder/retry/restart failures, add the
   command and invariant to the model-based layer, not only one scenario fixture.
4. Make the smallest production change that restores the invariant.
5. Run the focused red→green test, the package suite, then `make all`.

Every bug found in a real conversation becomes a permanent scenario fixture.
Sanitize content if necessary, but preserve event order, concurrency boundary,
and expected rendered transcript. A regression test that replaces the trace with
a direct helper call is insufficient unless a scenario already protects the
original observation path.

## Adversarial scripted models

Prompt instructions are usability guidance, not a correctness boundary. Scenario
tests must include scripted models that violate the advertised happy path:

- issue `task` and `sleep` together;
- issue `send_to_subagent` and `sleep` together in one parallel tool batch;
- issue `sleep` on the next iteration while a background child is still pending;
- poll a diagnostic tool;
- send follow-up at terminalization;
- repeat an old tool call after restart;
- return text together with a suspending tool;
- emit duplicate or delayed completion signals.

The harness must either process these sequences safely or reject them explicitly
before side effects. “The model was told not to do that” is never an acceptable
test argument.

## Review checklist

For every protocol or lifecycle change, verify:

- the test observes a public/durable boundary, not only a private flag;
- production SQLite and migrations are used instead of SQL mocks;
- synchronization uses conditions/channels, not arbitrary sleeps;
- duplicate, stale, and reordered delivery are covered where applicable;
- restart is tested at the persistence boundary when recovery semantics changed;
- user-visible assertions are made after the final renderer, not only at PubSub;
- no test encodes an accidental behavior merely because the current code does it.

## Current harness suite

These names are stable entry points. Extend them instead of inventing a parallel
testing convention:

- `internal/sessionstore/harness_model_test.go` — reference protocol model versus
  migrated SQLite for inbox acceptance/consumption, activation finalization,
  duplicate/stale delivery, crash windows, and restart;
- `internal/daemon/*_scenario_test.go` — one shared real daemon/session loop and
  controller-visible golden conversations, including adversarial tool batches,
  all-wait scatter/gather, sleep interruption, and restart after normal input was
  promoted either before an assistant response or after durable tool progress;
  daemon integration tests separately cover schedule ack-redelivery;
- `internal/managers/telegram/harness_scenario_test.go` — production Telegram
  rendering and exact Bot API request trace;
- `cmd/coagent/harness_e2e_test.go` — compiled daemon, Unix control socket, fake
  model catalog/LLM server, real SQLite and CLI manager (`integration` tag).

The model protocol also includes scheduled-delivery duplicate, conflict, fresh
reset and restart commands, and a `compact` command interleaving whole-transcript
compaction with deliveries, restarts and stale completions. The slow mutation gate covers tool execution,
session transcript delivery/reset, schedule execution/cleanup, and the durable
scheduled-delivery store; a change to one of those boundaries must remain in a
mutation scope rather than relying on statement coverage.

Useful focused commands:

```bash
go test ./internal/sessionstore -run 'TestHarnessModel|FuzzHarnessProtocol'
go test ./internal/daemon ./internal/managers/telegram -run TestHarnessScenario
go test -tags=integration ./cmd/coagent -run TestHarnessE2E
go test ./internal/sessionstore -run '^$' -fuzz FuzzHarnessProtocol -fuzztime=30s
make mutation MUTATION_PATH=./internal/session
```
