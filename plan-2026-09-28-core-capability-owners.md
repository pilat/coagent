# Core capability owners

## Goal

Phase 2 of the [core refactoring goal](docs/refactoring-goal.md): remove model
resource and external-call protocol responsibilities from the broad session
and daemon receivers. Move state with complete transitions so consumers cannot
independently mutate parts of one invariant. Baseline: `a231abc` on
`refactor/runtime-ownership`. This phase advances session independence,
traceable ownership and agreement between execution and recovery; it does not
complete the overall core goal.

## Decisions

1. Keep both owners private in their existing packages. Do not add dependency
   tiers, packages, framework wiring, new persistence or exported product APIs.
   Use consumer contracts and explicit construction dependencies; neither
   owner may retain a pointer to its enclosing svc.
2. A session model runtime owns the client, model/reasoning identity, config
   and client factory, resource lease mutex, generation and provider-measured
   context baseline. Generation and measurement remain under the client-swap
   lock: extracting baseline into an independent field bag would split the
   invariant that measurements describe the current model.
3. Model runtime performs validation, initial client setup, switching,
   resource-safe Chat/Close, metadata reads and baseline restore/save/clear.
   Supply prompt/search projection, live registry and image authorizer directly;
   supply only baseline persistence capability rather than the whole session.
   Preserve session/root provider grouping, default reasoning, native-search
   guidance and current error text. Consumers call this owner directly;
   svc retains only public Service delegation and necessary construction glue.
4. An external-call coordinator in daemon owns the staged-call ledger, config
   applier, activation settlement and authoritative pending-call projection.
   Staging is broader than config: stop and orphan recovery temporarily adopt
   task/sleep/config calls before opening transcript settlement. Move that
   adoption and settlement together. The coordinator consumes producer ledgers;
   schedules and child links remain durable authorities in their own packages.
5. The coordinator owns config claim, durable-suspend verification, apply,
   rejection/abandon settlement and grant consumption/expiry. It also owns
   transcript-only opening and per-session stopped/orphaned/interrupted call
   settlement. Startup sweep ordering, tree fences, runner lifecycle and
   input delivery remain with their current owners. Supply typed delivery
   effects and shutdown observation; callbacks must not delegate the actual
   staged-call or grant transitions back to svc.
6. Construction passes the config applier into the coordinator once. Replace
   late assignments in daemon.New and test fixtures; do not add setters or
   mutable svc discovery. Optional absent applier still disables config editing
   while ordinary external-call settlement remains available.
7. Preserve the existing SQL transaction and caller-fence boundaries. The
   concrete sessionstore receiver owns only db, and consumers already have
   narrow contracts. Splitting that receiver by table is not part of this phase.

## Constraints and ordering

- Model Chat holds a read lease for the entire provider call; switching and
  close cannot close its client until that call returns. Client construction
  failure leaves the current model usable. A stale generation cannot reinstall
  a measurement after a model switch. Metadata access does not expose a raw
  resource-using client to consumers.
- Close is terminal for client ownership. Existing daemon routing can sample a
  live session before teardown closes it while replacement construction is
  still in progress. The owner must reject installation after close and close
  that uninstalled replacement, rather than leak a client in a dead session.
  Protect this ordering with a deterministic constructor-barrier regression.
- Never acquire the transcript mutex while holding the model lock: compaction
  already holds the transcript mutex when it calls the model or invalidates a
  baseline. Summarizer usage must not become the ordinary request measurement.
- Normalize session/root identity before configuring the model owner. Build
  prompt and registry in their established order; native-search state precedes
  initial tools-section generation. Restore measurements only for the same
  model. Successful compaction/reset clears memory and persisted measurement;
  failed/no-op compaction preserves it. Persistence remains best-effort.
- Apply only when the transcript durably contains the unresolved named call.
  Claim before tool suspension; a second session cannot steal the global slot.
  A rejected apply retains call ownership until durable result delivery.
- Abandon releases the apply slot before lifecycle teardown, but transcript
  mutation waits for the caller's tree fence. Expire an abandoned grant before
  inserting its result so crash recovery cannot leave a grant blocking the FIFO.
  Stop settlement follows writer joins; recovery opens no model or tool stack.
- Keep pending-marker, schedules and child-link merge precedence and error
  behavior. Keep ordinary and external interrupted-call distinction, recovery
  pass ordering and existing notifications exactly as they are.
- Preserve all temporal test scenarios when replacing fixture mechanics. No
  weaker nil-selected algorithms, golden regeneration, new migrations, provider
  protocol changes, sandbox changes or real home/credential access.
- Use apply_patch and mise. Preserve current documentation edits. No new
  staging, commits, pushes or PRs without a specific user instruction.

## Tasks and acceptance

### T1: Session model runtime

Affected: internal/session/model.go, serialize.go, session.go, run.go, loop.go,
compaction.go, compaction_summarize.go and construction/model/context fixtures.
Add a private model owner and update every consumer of the former fields.

Acceptance: svc no longer owns client/config/factory/model mutex/generation/
baseline fields; loop and summarizer lease the owner; model tests can exercise
the owner without constructing unrelated session state. No private forwarding
facade recreates the old receiver's model API. Keep pure token estimates
independent of the owner. Prove in-flight Chat versus switch/close ordering,
stale measurement rejection, construction reasoning/grouping/authorization,
same-model restore and compaction/reset invalidation using existing scenarios.

Focused evidence includes TestHandleSetModelWaitsForInFlightChatBeforeClosingOldClient,
TestBaselineFromAnInFlightRequestIsDroppedAfterAModelSwitch,
TestContextBaseline_CompactionClearsThePersistedRow, the existing model-switch
and reasoning tests, and daemon compaction/model scenarios affected by wiring.
Adapt names only when the new owner changes the tested entry point; preserve
the behavior assertions. Add deterministic ordering coverage where existing
tests do not cover the new ownership boundary.

### T2: Daemon external-call coordinator

Affected: internal/daemon/{manager,runner,staged,config_tools,orphan_calls,
interrupted_calls,input_recovery}.go; stop/completion callers, construction and
config/restart fixtures; cmd/coagent or manager tests only if constructor
integration requires it. Shared Service interfaces remain compatible.

Move staged storage, its consumers and complete config/settlement transitions
to a private coordinator. Keep startup sweep loops with lifecycle integration,
but have them invoke per-session coordinator operations. Change private callers
to use the owner directly. Preserve public boot hooks as narrow delegation.
Move exact pending-result resolution and ledger retirement together: normal
owned-result delivery retires ownership only after resolution succeeds. Keep
the existing distinct cleanup rules: orphan-recovery adoption is temporary and
is always released on return, including failed transcript open/resolution;
unbacked apply releases ownership when the call is demonstrably absent or the
daemon is shutting down. Retaining either would invent a surviving producer.
Preserve causal-result-before-input ordering and awaited sender acknowledgements.
Pass a narrow shutdown observer as an operation argument where needed instead
of exposing mutable daemon state. Read it at the existing cleanup decision
after transcript I/O; an entry-time bool would miss concurrent shutdown.

Acceptance: svc has no staged ledger, applier or activation-store field; no
remaining code reaches into the coordinator's ledger to complete a transition.
Config tool construction consumes the coordinator's capability rather than svc.
No new executable session is built for recovery. Verify generic external-call
adoption as well as config. Remove unused StageExternalCall if repository-wide
caller inspection confirms it is absent from every consumed contract.

Focused evidence includes TestRunStagedApply_HandsOverExactlyOnce,
TestRunStagedApply_RefusesToCommitForASuspendTheTranscriptDoesNotCarry,
TestScenario_ASecondSessionCannotOverwriteAStagedApply,
TestScenario_AbandonedApplyRetriesFailedResultOnExplicitInput,
TestScenario_ConfigApplyVerdictRedeliveryIsIdempotent,
TestScenario_BlockingTaskOrphanedByARestartIsClosedOnce,
TestScenario_InterruptedCallSettlementNeedsNoProjectOrModel, and stop/restart
grant scenarios. Trace consume-after-commit through cmd/coagent boot callers.

### T3: Integration and evidence

Review combined changes and verify dependency/state removal with source
inspection, not just counts. Run focused tests per coherent checkpoint; do not
run complete lifecycle package suites locally. Follow project final cadence:
one local make test, cold spec review, delegated lint/fixes, reporting-only
subagent canonical CI under the explicit AGENTS.md implementation exception,
then final make all. Never run mutation or shorten canonical gate budgets.
Run independent code review under the applicable handoff workflow and report
its status separately from specification review.

Sync ARCHITECTURE.md and add an ADR if the final boundary introduces a
significant design tradeoff. Update docs/refactoring-goal.md with completed
ownership, exact validation status and remaining work. Evaluate next-phase
context/compaction, tool execution and daemon routing candidates against the
full goal; do not declare the core complete because T1/T2 passed.

## Out of scope, accepted

- Full compaction/context and tool-execution extraction: follow the model
  boundary so those designs consume an established resource owner.
- Daemon publication ownership and general input routing: independent protocols
  remain visible as follow-up candidates, not silently transferred into the
  external-call coordinator.
- SQL receiver decomposition and query optimization: no demonstrated ownership
  gain in this phase; cross-table atomicity remains essential.
- Telegram decomposition: separate manager workstream outside the core goal.

## Execution record

- Exploration: model lease/baseline and external-call/config settlement owners
  identified by state access and temporal protocol tracing.
- Plan validation: passed after clarifying normal result retirement versus
  temporary orphan adoption and unbacked-apply cleanup. Existing failure paths
  remain explicit; no behavior change was selected.
- Integration inspection: preserve shutdown observation after transcript I/O.
  Model-lifetime tracing also exposed a pre-existing close/switch race: a
  replacement built concurrently could be installed after session teardown.
  T1 includes terminal close and a deterministic regression as part of making
  resource lifetime owned; normal model selection and error text stay intact.
- T1: complete. Model/construction/baseline checks passed (8.412s), twenty
  temporal/fixture cases passed (142.579s), and the ordinary real-SQLite
  measurement assertion passed (2.344s). Durable loop fixtures construct the
  store and session identity before their model owner. Terminal close rejects
  and releases a late replacement; the deterministic regression passed.
- T2: complete. Existing config/recovery/stop and ownership-model checks passed
  (21.037s), additional config/restart/gating cases passed (8.385s), and new
  orphan cleanup, failed-result retention and shutdown-during-read regressions
  passed (2.410s). Public daemon construction remains compatible.
- T3: final gates/reviews pending combined phase-2/phase-3 integration; phase 3
  continues from each completed package checkpoint without an intermediate
  handoff or redundant whole-suite run.
