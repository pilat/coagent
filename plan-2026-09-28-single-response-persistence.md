# Canonical response and tool-result persistence

## Goal

Close the remaining competing-response-algorithm finding in the
[core refactoring audit](docs/refactoring-audit.md), advancing the
[overall goal](docs/refactoring-goal.md). Production already uses
CommitAcceptedResponseDisposition; the obsolete InsertBudgetedResponse remains
exposed and tested despite having no production callers.

## Decisions and constraints

- Delete BudgetedResponse, BudgetedResponseResult, BudgetResponseStore and
  InsertBudgetedResponse, including insertBudgetedOutput,
  insertBudgetedProgress and fireBudgetedResponse, after confirming no callers
  outside the obsolete path. Remove the capability from AgentRuntimeStore.
- Retain budgetCrossing, budgetCrossingReason, roundCostUSD,
  budgetToolNotExecuted and insertBudgetNonExecution. Crossing helpers already
  serve production; non-execution insertion currently serves only the obsolete
  path and must move into the canonical transaction, not disappear with it.
- Restore the existing ADR-0033/architecture contract in the canonical path:
  every accepted response observes the budget once inside its transaction,
  independently of whether it carries output. A crossing or already-fired
  budget atomically records non-execution results for returned tools. Retain
  the host-only checkpoint and existing generation fencing. Empty-stop nudges
  and other ordinary outputs are suppressed behind a fired budget.
- Make budget observation a common part of the accepted-response transaction;
  disposition-specific helpers consume its result rather than independently
  observe it again. Preserve their candidate CAS, iteration and reply-obligation
  semantics. The loop adopts the fired verdict without normal final-answer,
  projection-error or empty-terminal side effects overriding parking.
- Migrate the crossing/late-response scenario and the operator protocol model
  to CommitAcceptedResponseDisposition with explicit tool-call dispositions
  and iteration identity. Preserve accounting, one checkpoint, non-execution
  results, already-fired suppression and release-on-next-model-input assertions.
- Strengthen the reference model to derive firing from accumulated input costs
  rather than echoing the implementation's fired result. Assert exact expected
  non-execution counts and absence of suppressed model prose in the checkpoint.
  This verifies the production policy; do not preserve the obsolete algorithm's
  different output concatenation behavior.
- Remove unused PersistResponse methods on test budget gates if no interface
  requires them. Coordinate with active session work; do not overlap edits.
- The intentional behavior correction prevents tools returned by a crossing
  response from executing and keeps budget parking ahead of normal termination.
  This restores the recorded budget policy; it does not choose a new limiter.
  No schema migration, general SQL receiver split, provider change or other
  budget-policy redesign. No weakening of temporal scenarios.
  Use apply_patch/mise; no staging, commits, publishing or live credentials.

## T1: Canonical budget response and acceptance

Affected: internal/sessionstore/budget_response_store.go,
transaction_owners.go, completion_check_store.go, operator_protocol_model_test.go,
budget_store_test.go; session disposition post-commit adoption and obsolete
test-double methods only by its current owner; a focused daemon scenario.

Remove the unreachable algorithm and migrate tests. Search the whole repository
to prove obsolete identifiers have no code references and shared production
helpers remain called. Run exact TestOperatorProtocolModel_ParallelCrossingReleaseAndReplay,
TestBudgetStore_CrossingResponseCommitsUsageNonExecutionAndCheckpointTogether
and TestBudgetStore_CompactionCrossingCommitsReplacementAndCheckpointTogether.
Keep real migrated SQLite. Preserve paid attempts and deterministic orderings;
do not replace these scenarios with pure helper tests.
Run the migrated tests before correcting production to establish the failure.
Add a canonical response matrix covering outputless tools, direct replies,
empty stops, candidate/confirm/yield/projection-error budget precedence and a
transaction rollback case. Add a real daemon/session scenario proving a
crossing response never executes its returned tool and parks with one host
checkpoint. Include a terminal-empty case proving normal notices cannot
override the fired outcome. Do not rely on timer timing as the oracle.

Acceptance: production and protocol tests share one accepted-response
transaction; no alternative budgeted-response capability survives; the
independent cost model agrees with durable facts through crossing, late
responses and release. Final integrated spec/code reviews and gates cover this
task alongside phases 2 and 3, without redundant whole-suite runs.

## T2: Remove remaining alternate output transactions

Follow-up caller tracing found two related duplicates. Remove
InsertAssistantMessageWithOutput and AssistantOutputStore (including its
embedding and duplicate OutputStore declaration), and remove
InsertToolResultWithDirectOutput from DirectOutputStore and its implementation.
No production execution uses either transaction; the assistant helper chain
is itself only reached by tests. Keep constants and shared output helpers still
used by canonical dispositions and host/command output.

Migrate assistant output/generation/fence tests to explicit accepted-response
dispositions: tool replies use ToolCall with their output type; releasing
output uses the existing Confirmed or BackgroundYield contract as appropriate
to the scenario. Preserve source-key identity, model-input generation,
reply-obligation and lifecycle-fence assertions. Do not replace the tested
atomic operation with separate InsertMessage and EnqueueOutput calls.

Migrate single tool-result scenarios to one-entry InsertToolResultSetOnce,
preserving ownerless/child degradation, stop fences, replay/conflict, fresh
completion-check invalidation and no invalidation on replay. Test-only result
unwrapping helpers may reduce mechanical repetition but contain no transaction
or protocol decisions. Keep batch validation and idempotency behavior authoritative.

Within session, remove production addAssistantMessageOutput and
appendAssistantOutputLocked. Move plain addAssistantMessage seeding to a
test-only helper retaining all serialized response fields. Preserve
TestMessageStore_FinalPromotionTargetsLastAssistantMessage by creating its
assistant rows through canonical disposition commits and reloading the
projection. Do not remove enqueueFinalAssistantOutput: loop.finalize still
uses it for the hard iteration ceiling, a distinct host termination operation.
Coordinate session edits with the active package owner.

Affected: sessionstore/output_message_store.go, output_types.go,
direct_output_store.go and direct-output, output-store, progress-generation,
generation-protocol, stop-completion, ingress-completion tests; session's
message_output.go and fixture/final-output tests. Run their exact affected
test names and model traces. Inspect any failures against the production
contract before changing assertions. No unrelated SQL cleanup or schema change.

Acceptance: assistant, single-result and batch protocol tests call the same
transactions as runtime; obsolete capabilities and algorithm bodies are absent.
Keep legitimate host output and command/lifecycle output operations intact.

## Out of scope, accepted

The shared SQL receiver remains: its db handle is not a competing state owner.
Rejected attempts and compaction have different transaction contracts and are
not removed as apparent duplication.

## Execution record

- Caller inspection: three call sites in two test scenarios, none in production.
- Cold review found the production path missing unconditional budget observation
  and atomic non-execution rows; the obsolete tests masked that gap. Plan now
  explicitly restores the existing contract while removing the obsolete path.
- Follow-up plan validation passed. Canonical implementation and obsolete API
  removal are complete. Migrated tests failed before the fix (1.405s); the new
  disposition/rollback matrix also failed (6.156s), exposing skipped observation,
  missing non-execution and missing projection-error budget ownership.
- Focused canonical budget/model/candidate/compaction tests passed after the fix
  (10.207s). The real daemon scenario reproduced two premature tool executions
  before correction (1.766s), then passed with zero executions, one checkpoint,
  preserved cost and explicit-input release/resume (0.786s).
- Terminal-empty budget precedence passed in the focused session selection
  (3.711s); integrated reviews/gates remain pending.
- T2: complete. Both alternate APIs and production assistant helper chains are
  removed. Migrated atomic-output/generation/replay/fence tests and protocol
  models passed (13.351s), ordinary generation fuzz seed corpus passed (3.354s),
  and final assistant/generation checks passed after deletion (5.667s).
  Canonical final-promotion fixture passed (0.912s). No obsolete Go references
  remain; legitimate host final promotion remains in use by the loop.
- Independent code review found a post-commit adoption gap: a failed transcript
  reload after a paid crossing used to skip park scheduling and report a generic
  session error. A real-SQLite regression failed before the correction, then
  passed with budget precedence, one park callback, one host checkpoint, stored
  non-execution, no tool execution and a suspended rather than error status
  (3.327s). The owner now adopts the committed verdict before reloading; a
  failed read cannot override the parked outcome.
