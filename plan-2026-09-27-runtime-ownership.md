# Runtime ownership refactor

Phase 1 of the [core refactoring goal](docs/refactoring-goal.md).
Implementation is committed as `a231abc`; independent code review is pending.
The overall goal remains active beyond this phase.

## Goal

Reduce the runtime's concentration of state and decisions without changing its
observable task, stop, recovery, completion or delivery behavior. This branch
finishes concrete ownership boundaries around runner management, construction,
session response decisions and budget enforcement. Preserve the modular
monolith and the existing SQLite transaction boundaries. Success means fewer
independent owners of one invariant, not merely smaller source files.

Baseline: `7678d2c3d592f1a9d09998754b7be45b55f9beff`.
Work branch: `refactor/runtime-ownership`.

## Decisions

1. Use existing packages and cohesive private types wherever possible. Keep
   sessionlifecycle as the owner of runner coordination. Do not introduce a DI
   framework, event bus, generic workflow engine, actor system or repository per
   table. The daemon remains the integration/application boundary.
2. Move state together with the operations maintaining its invariants. A
   lifecycle supervisor owns registry/admission coordination, overflow queues,
   their retry worker, and tree fencing. Daemon adapters may supply session
   assembly, durable startability and producer effects, but cannot mutate the
   supervisor's registry or admission counters directly.
3. Complete the tree-stop orchestration in a lifecycle-owned operation over
   explicit effects: signal every runner, cancel/join processes, await runners,
   retire resources, settle inputs/calls/sleeps, then finish durable stop. Keep
   stop and recovery on the same operation. Retain the existing durable Stopper
   transactions and producer-specific recovery semantics.
4. Required capabilities are explicit construction inputs. Construct process
   storage/service and resource ownership visibly; never discover the database
   through a project repository or install/close resources through a concrete
   factory assertion. Session-scoped creation inputs can bind runtime callbacks
   to break construction feedback without mutating a partially built factory.
5. One implementation owns completion-check invalidation. Require the existing
   sessionstore transaction function as a constructor dependency of subagent
   transactions; remove its default duplicate and late setter. This keeps the
   import graph acyclic and preserves the active SQL transaction.
6. Executable sessions use one accepted-response transaction path. Pure policy
   tests remain cheap; durable loop tests use real temporary SQLite or an
   explicit transaction-contract test double. Remove nil-selected legacy turn
   persistence instead of maintaining a second protocol for tests. Lightweight
   transcript-only helpers need not construct executable sessions.
7. Extract response classification into a pure decision function with explicit
   facts (finish/tool shape, candidate, wake verdict, empty streak and reply
   obligation). Keep accounting, budget precedence and CAS validation in the
   existing atomic disposition transaction. Do not move network work into SQL
   transactions or introduce a read/decide/write race.
8. Expose a canonical recovered activation outcome from the existing durable
   session/message evidence. Child completion consumes that outcome instead of
   reimplementing candidate/rejection/empty-stop interpretation. No new outcome
   ledger or schema migration is required. Compaction continues to protect the
   candidate/nudge row identities and never summarizes them away.
9. Budget duration/deadline reconciliation belongs to execution control, not
   progress rendering. Move its clock, startup reconciliation and park trigger
   to a lifecycle-owned component. Progress reads committed budget facts and
   keeps its existing rendering/readiness semantics. Response cost crossing
   still commits with the response transaction.
10. Preserve externally visible text, manager ownership, command behavior,
    at-least-once transport delivery, background yielding and all existing
    fences. Do not conflate session status, live runner state, child activation
    sequence or manager-output acknowledgement.

## Constraints and critical ordering

- One accepted input remains pending, handled, rejected or cancelled across
  restart. Notifications and in-memory queues are hints/caches, never the ledger.
- Stop intent fences producers before cancellation. Signal every runner before
  joining any runner. Runner Done must become observable while stop holds the
  tree fence; child finalization that needs that fence happens afterward.
  `TestFinishRunnerCancellationEscapesContendedTreeFence` protects this boundary.
- Registration vs shutdown and append vs deregistration/drain remain serialized.
  Release exactly one admission slot per registered runner. Capacity queues keep
  their current FIFO/eligibility and durable-recovery behavior.
- Parent completion and ledger acknowledgement remain one transaction. A stale
  child activation cannot resolve a newer round. Background events enter the
  inbox between complete tool batches; exact foreground results retain their
  call-bound protocol.
- Candidate+nudge, confirmation+answer identity, accounting, budget observation
  and output retain their current atomic contracts. Confirmation publishes the
  candidate, not the confirming acknowledgement. Paid attempts remain recorded
  even when rejected or suppressed. Existing database fences remain authoritative.
- Preserve interrupted-call settlement, config-apply marker/grant ordering,
  tool gating, source ownership and process guardian behavior.
- Resource shutdown follows runner/process joins. Transcript-only settlement
  must not create a model client or tool stack.
- No credentials or real home-state access. Use mise toolchains and apply_patch.
  No staging, commits, pushes, PR creation or publishing are authorized.
- Preserve scenario/model/renderer coverage when changing test mechanics. Do
  not regenerate golden output to conceal behavioral changes.

## Scope boundaries

This branch implements the runtime changes below. Telegram API/topic extraction,
project-store relocation, wholesale sessionstore receiver splitting, query
optimization and package-wide naming cleanup are separate follow-ups: they do
not need to change to establish these execution boundaries. Provider protocols,
sandbox policy, public product behavior, database schema and migration history
remain outside this refactor. No claim is made that every oversized object is
eliminated by this branch.

## Tasks and acceptance

### T1 — Explicit construction and transaction dependencies

Affected: cmd/coagent/main.go; daemon/manager.go, store.go, process_cancel.go,
runner.go and construction fixtures; session/factory.go and factory_create.go;
subagent/transactions.go and all constructor callers.

- Inject process store/service and tool-resource lifetime capabilities through
  named construction inputs. The tree fence/wake callback must remain bound to
  the same lifecycle authority used by stop and process launch.
- Remove DB discovery on daemon.Store, factory decoration by concrete assertion
  and resource helper functions that silently no-op for other Factory types.
- Make response-disposition dependencies explicit, and replace the subagent
  invalidation setter/default SQL with the required constructor dependency.
- Separate transcript settlement capability from an executable session's
  construction contract where it currently requires a partial session object.

Acceptance: wrappers do not silently lose process/resource capabilities; normal
startup and failure cleanup preserve ownership; canonical invalidation executes
inside parent-delivery transactions; no removed runtime discovery remains.
Focused evidence: factory/resource tests, one background process scenario,
config abandonment/stop settlement, and subagent completion protocol cases.

### T2 — One response algorithm and explicit completion meaning

Affected: session/loop.go, completion_policy.go, response_integrity.go,
factory_create.go, session.go, budget_gate.go and focused session tests;
sessionstore completion/outcome projection; sessionlifecycle/response_integrity.go;
daemon budget adapter only as required by the contract.

- Introduce the pure decision function and explicit input/output types without
  transferring transaction authority to the loop.
- Remove legacy accepted-response and empty-stop paths selected by missing
  dispositions. Adapt old lightweight tests to policy tests or the production
  transaction contract, preserving the behavior scenarios they cover.
  Existing temporal loop tests must script candidate and confirmation turns
  while retaining their Working, notification and input-order assertions.
- Centralize recovered outcome interpretation; lifecycle uses its typed result.
  Keep confirmed-candidate pointers, rejected attempts, terminal empty-stop
  notices and background-yield answers unchanged.

Acceptance: no executable turn selects its algorithm by optional persistence;
live and recovered result semantics agree; accounting and budget suppression
remain atomic. Verify candidate confirmation, tool invalidation, stale candidate,
empty streak, rejected responses, compaction pinning and restart scenarios.

### T3 — Lifecycle supervisor and complete stop coordination

Affected: sessionlifecycle/{launcher,registry,queue,runner,stopper,recovery}.go;
new supervisor/tree coordination files within that package;
daemon/{manager,runner,admission,pending_runner,queued_stop,tree_lock,completion,
completion_wiring,spawner,process_completion,budget_park}.go and their callers/tests.

- Put registry/admission/queue/fence state and queue retry lifetime behind one
  supervisor with consumer operations, not exported raw containers.
- Move registration/release/drain coordination and tree stop phases under that
  owner. Daemon callbacks express producer/session effects and do not reach back
  into supervisor internals to complete its transitions.
- Keep cancellation completion separate from fence-dependent finalization.
  Preserve ordinary restart and process-specific recovery ordering through
  explicit lifecycle methods; do not invent a second recovery state machine.

Acceptance: daemon has no registry/queue/governor/tree-lock fields and cannot
  mutate those primitives; stop and interrupted-stop recovery call the same
  coordinator; register/shutdown, append/teardown, stop/spawn, queued roots and
  children, budget park and stale completion cases retain their behavior.
Existing exact tests plus protocol-model/scenario cases provide evidence.

### T4 — Budget execution independent of progress

Affected: progressruntime/{runtime,reconciler,budget_recovery}.go and tests;
sessionlifecycle budget reconciliation component; daemon/progress.go,
budget_park.go, completion.go, manager.go and budget scenario fixtures.

- Move armed-budget recovery, duration deadline scheduling and park triggering
  out of progressruntime. The new owner starts and stops with runtime lifecycle,
  wakes on budget changes and reconstructs deadlines after restart.
- Avoid losing a duration deadline when there is no active model call or when
  progress capture fails. Preserve one-shot generation fencing and join workers
  on shutdown. Reuse budget.Service.Observe, which already reads current cost
  transactionally; do not add an independent cost decision or require progress
  capture before observation.
- Progress still displays budgets and drops stale cards under existing output
  generation rules, but has no budget mutation or park capability.

Acceptance: duration budgets fire without a running progress renderer; restart
  reconciles armed/half-parked generations; stopped roots and background work
  preserve existing budget retention/release policy; no progress->budget-control
  dependency remains. Test deadline, shutdown, restart and progress scenarios.

### T5 — Integration, architecture and handoff

- Review the combined diff against every constraint and task. Update the
  architecture package map/ownership profiles and glossary only for actual
  changes. Update .go-arch-lint.yml to permit only the resulting dependencies.
  Record the significant ownership choice in the next ADR, building on ADR-0038
  rather than erasing it.
- Run focused tests at coherent checkpoints. Do not run full package suites or
  linters after each edit. At the end run the full local test gate once, then
  cold specification review against this plan and integrate confirmed findings.
- Delegate final lint and any lint fixes to a same-model subagent. Delegate the
  canonical `CI=true make ci` run to a same-model reporting-only subagent under
  the explicit implementation-session exception in the supplied AGENTS.md;
  never set CI=true in the primary session or run mutation testing.
- Run make all as the final local handoff gate. Avoid rerunning a successful
  test stage unnecessarily if make all already invokes it: retain exact gate
  evidence and identify reuse explicitly. Investigate failures with the specific
  failing target before one final rerun. Read exit status and logs, never pipe a
  gate to tail.
- Follow pilat:handoff-review: cold spec review precedes the separate user gate
  for cold code review. All implementation and checks finish before that gate.
  Leave local edits unstaged and report remaining review status accurately.

## Execution record

- Plan: cold validation passed; no consequential findings. Temporal-test
  preservation and reuse of transactional budget observation clarified.
- T1: complete. Explicit factory-wrapper resource/process regression and atomic
  invalidation rollback/duplicate-delivery checks passed with real SQLite.
- T2: complete. Legacy temporal fixtures now exercise the disposition contract;
  policy, recovered outcome, candidate, empty-reply eligibility, compaction,
  reasoning, input-order and notification checks passed.
- T3: complete. Focused stop-fence, queue, shutdown, process, config recovery
  and supervisor protocol cases passed.
- T4: complete. Independent deadline, restart, wake and joined shutdown tests
  passed. Budget tool commit-before-wake and progress projection cases passed.
- T5: full local make test passed. Subagent delivery/re-arm/restart/compaction
  model cases, production Telegram delivery scenarios and composition-root
  lifecycle checks passed. Independent specification review found no T1-T4
  mismatch. Final lint passed. The first CI run stopped at the architecture
  check: passing the full built-in resource interface into daemon exposed an
  implementation dependency. The composition root now publishes a neutral
  tool.ResourceLifecycle capability separately from stack access, preserving
  one resource instance and unchanged architecture rules. Focused make arch and
  final make lint passed after this correction; specification follow-up was
  clean. CI then rejected three panic guards; constructors now return explicit
  errors and invalid tree-lock state returns an error. Failure-path regressions,
  caller integration, lint and focused Semgrep passed. The full canonical CI
  rerun passed with exit 0: all static checks and 51 package suites passed,
  including daemon, session, sessionstore and integration tests. Final make all
  passed with exit 0; unchanged local test results were reused from Go's cache
  where available. Implementation, architecture sync and required checks are
  complete. Cold code review remains a separate user-approved stage.
