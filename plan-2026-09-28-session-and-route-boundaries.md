# Session execution and manager-route boundaries

## Goal

Phase 3 of the [core refactoring goal](docs/refactoring-goal.md), building on
[phase 2](plan-2026-09-28-core-capability-owners.md). Give tool turns, context
checkpoints and manager routes complete owners so session and daemon coordinate
capabilities rather than share their mutable state. Preserve visible behavior,
durable protocol ordering and the modular monolith. Phase 2's focused acceptance
must pass before implementation starts in its respective package.

## Decisions

1. Use private components inside session and daemon. Reuse the existing model
   runtime, messageStore, tool scheduler and durable transactions. Components
   receive direct capabilities and operation inputs, never a svc pointer or a
   callback that merely re-enters svc to perform their own transition.
2. A session tool-turn owner owns loop detection and execute-to-commit policy.
   The shared internal/toolexec package remains the scheduling primitive. Move
   stable registry resolution, activation-only batch validation, result/error/
   suspension classification, truncation/framing, detector accounting, ordered
   batch persistence and post-commit progress into the private owner.
3. Activation acquisition, terminal expiry and cancellation stay in the loop's
   input protocol. Pass the active grant into a tool turn and return a typed
   result carrying suspension and consumed-grant outcome. The owner must record
   successful consumption before a subsequent progress error is returned, so
   the caller can adopt the committed state even on that error. Do not clear a
   grant or publish success before durable result commit.
4. Replace external detector-field access with operations expressing policy:
   whether model requests may include tools, observation of a tool-free reply,
   and reset after new input/context replacement. The ordinary call and
   summarizer use the same verdict. Input/loop semantics stay at their boundary.
5. A session checkpoint owner owns compaction request/command state and the
   complete checkpoint attempt. Move focus, pending input, deferral episode,
   successful summary identity, automatic-attempt counter and disable state
   with pressure decisions, split selection, summarization, positioned commit,
   command completion and output publication. Run-local attempt state resets
   each run; the carried deferral episode keeps its existing resume semantics.
6. The loop retains selection of its safe compaction point and sleep
   interruption before a compact command. The owner independently refuses
   unresolved calls. Supply transcript, model, prompt/schema policy, completion
   reader, budget gate, output/progress/input-command capabilities and live
   background readers directly. Pass pending-call facts derived by the existing
   transcript classifier without letting the owner mutate staged producer state.
   Return a typed budget-fired outcome; the loop adopts it without the owner
   reaching into the loop's flags.
7. Reuse one messageStore projection and its durable replacement operations.
   Fresh-context reset coordinates replacement plus model-baseline invalidation
   through the same context owner; TODO and detector reset happen only after a
   successful inserted replacement. Transcript-only recovery continues to use
   messageStore without constructing model or checkpoint owners.
   The checkpoint outcome distinguishes durable commit from subsequent
   presentation effects. After a valid committed replacement, finish baseline,
   summary identity and command-state adoption and return the budget verdict
   even if progress publication fails. Log that publication error separately;
   do not report compaction failure, re-complete an already settled command or
   increment the non-relieving-attempt count for that successful checkpoint.
8. A private daemon manager-route owner owns publication cache/mutex, manager
   claim/replacement mutex, attribute changes, replacement resolution and
   owner-aware root replacement. It consumes route persistence, manager-root
   transactions, project metadata and the event bus. General runtime and
   session-input policy remain daemon responsibilities.
9. A replacement operation spans durable replacement, clear notification and
   old-root retirement under the ownership lock. Its caller already holds the
   tree fence; supply a narrow retirement effect for the lifecycle operation.
   This preserves tree -> route -> cache lock order and rejects late claims on
   the old root. Do not return a half-finished transition or expose Lock/Unlock.
   Publication must not take the ownership lock, since retirement publishes
   events while that lock is held.

## Preserved contracts

- Tool plans execute the same registry instance they classified. Keep ordered
  stages, bounded parallelism, skipped-call results, suspension/cancellation
  exclusions and atomic result-set persistence. Detector accounting excludes
  skipped/suspended/cancelled calls; warnings attach to the same last persisted
  executed/failed result. Keep untrusted framing after fingerprinting and before
  persistence, direct-output limits and unchanged progress/notification ordering.
- Compaction retains the immutable header and verbatim legal tail, candidate
  and nudge row identities, current skill, native-prefix repair, background
  snapshot, image pressure and attributed summary usage. Keep existing
  budgeted/command/plain commit branches and command-output atomicity. Errors
  before commit, no-op and non-relieving attempts do not adopt replacement state.
  A post-commit projection failure cannot roll back or hide committed state.
- Keep transcript -> model lock order. Preserve existing errors, warnings,
  user-facing text, best-effort measurement writes and tolerated output errors.
  The explicit correction is post-commit progress failure: log it without the
  existing false compaction-failure output. Do not strengthen an uncommitted or
  no-op operation into a committed success.
- Compaction deferral remains in the durable inbox behind non-sleep external
  calls. Sleep can be interrupted first. Preserve automatic failure cap,
  one-shot focus, keyed auto-success output and stopped/budgeted behavior.
- Manager claims cannot remove/rebind an existing owner. A committed claim
  beats a stale publication read; failed lookups do not poison cache. Child
  suppression and ownerless/fail-open bus behavior remain unchanged. Clear
  preserves ownership and resolves later input through the durable replacement.
- No schema/provider/sandbox/product change, new package tier or broad cleanup.
  Preserve behavior scenarios, not just helper signatures. Use apply_patch and
  mise; no live credentials/home state, staging, commits, pushes or PRs.

## Tasks and acceptance

### T1: Tool-turn execution owner

Affected: internal/session/toolexec.go, loopdetect.go, loop.go,
compaction_summarize.go, cut.go, session.go, loop_boundary.go and tool/detector
fixtures. Remove svc parameters from the entire tool pipeline and loopDetector
state from svc. Construction injects live registry, model-window query,
transcript result writer and progress capability. Notification can be an
explicit per-turn effect; it contains no policy.

Acceptance: all tool-turn policy and persistence sequencing reside in the
owner; the loop adopts its typed result and owns only activation/loop decisions.
Preserve TestExecuteToolCalls_Stages, TestExecuteToolCalls_PlannedInstanceSurvivesRegistrySwap,
TestExecuteToolCalls_PersistenceFailureLeavesNoPartialSet,
TestExecuteToolCalls_LoopDetectorIgnoresSkips,
TestExecuteToolCalls_WarningOnlyOnLastPersistedResult and
TestBatchFallbackParityWithNativeScheduling. Run exact activated-command,
suspension, untrusted-output, direct-output and TODO progress cases. Add an
ordering regression if existing tests do not distinguish committed grant
consumption followed by progress failure from failed result persistence.

### T2: Checkpoint and context owner

Depends on T1's schema policy. Affected: internal/session/compaction*.go,
serialize.go, cut.go, loop_context.go, loop_boundary.go, session.go, run.go,
loop.go and corresponding fixtures. Keep independent computation functions
independent; move the stateful algorithms, not only storage fields.

Acceptance: svc and loopRunner no longer own compaction state or implement
checkpoint transactions/command outcomes. The safe-point invocation is small;
the owner cannot read unrelated svc state. Reuse the existing transcript
primitive for both live and transcript-only consumers. Preserve reset
idempotency and baseline invalidation only after inserted reset.

Run exact TestCompactionProtocolModel and existing candidate-pin, native-prefix,
skill, image, cap, commit-failure, budget-row-ID and command-output cases.
Cross-layer evidence includes TestHarnessScenario_CompactSuccessChain,
TestScenario_CompactWaitsForABlockingChildThenRuns,
TestScenario_DeferredCompactSurvivesADaemonRestart and status/compact interruption
scenarios; retain their production renderer coverage. No golden regeneration.
Add deterministic real-SQLite regressions for committed checkpoint followed by
progress failure: baseline invalidation and summary identity remain correct,
explicit command succeeds only once, and a committed budget-fired verdict still
parks execution. This closes the discovered presentation/control coupling;
ordinary pre-commit error behavior remains unchanged.

### T3: Manager-route owner

Independent of T1/T2 after phase-2 daemon integration. Affected:
internal/daemon/{manager,publish}.go, new private route implementation,
publication/controller/claim/replacement fixtures and construction wiring.
Remove route/cache mutexes and maps from svc. Move attributes and full owned
replacement into the owner with explicit lifecycle effects, not callbacks
performing route decisions. Existing public daemon/controller contracts remain.

Acceptance: owner/cache mutation occurs only inside the route owner; daemon
holds tree fences and asks for a complete replacement. Preserve
TestPublishGate_ConcurrentClaimWinsOverAStaleRouteRead,
TestPublishGate_FailOpenDoesNotPoisonCache,
TestPublishRoutingModel_ManagerOwnershipSurvivesTransitions,
TestManager_SetAttributesCannotRemoveOrRebindManagerOwner,
TestManager_ClearRejectsAConcurrentLateOwnerClaim and
TestSendSessionMessageResolvedFollowsOwnedReplacement, plus real manager-owner
delivery scenarios. Cold-cache tests reconstruct the owner rather than mutate
private maps from daemon fixtures.

### T4: Full-goal audit and integrated verification

Inspect actual ownership after integration and trace all flows named in the
goal's completion criteria. Record a responsibility/flow evidence table and
remaining debt with reasons in the current goal checkpoint or a linked report.
If unrelated mutable-state concentrations remain, plan their resolution; these
tasks do not redefine the overall goal's completion threshold.

Focused tests follow coherent package checkpoints. Phase 2 and phase 3 share
one final integrated handoff when performed continuously: local make test,
cold spec review of both plans, delegated lint/fixes, reporting-only delegated
canonical CI under the AGENTS.md implementation exception, final make all,
and independent code review under the applicable handoff workflow. Keep exact
verification evidence and rerun successful gates only after relevant changes.
Never run mutation or shorten budgets. Sync architecture docs and record the
significant boundary decisions in ADRs. Leave code-review status explicit.

## Out of scope, accepted

- General daemon input routing and startup/shutdown: retained coordination
  pending the full-goal audit; this phase does not move unrelated application
  decisions into the route owner.
- Splitting the SQL receiver: retained narrow capabilities and cross-table
  transactions already provide the ownership boundary; size alone is insufficient.
- Telegram decomposition and product changes: separate from the core goal.

## Execution record

- Exploration: state/consumer tracing identified the three boundaries above.
- Cold plan validation: one finding clarified committed checkpoints versus
  post-commit progress failure. The final owner preserves committed facts and
  logs publication failure separately; focused regressions cover the correction.
  Follow-up validation passed with no remaining consequential findings.
- T1: complete. Focused staging, planned-instance, atomic result-set, detector,
  suspension, framing, direct-output and image cases passed. Corrected fixture
  forced-text-only cases and committed-grant/progress-failure regression passed
  (5.973s). The tool pipeline has no session receiver and owns its detector.
- T2: complete. Real-SQLite checkpoint protocol and post-commit progress-failure
  cases passed (9.552s); native-prefix, skill/image preservation, failure cap,
  command deferral and fired-command behavior passed (3.711s). The owner adopts
  committed state despite publication failure; the loop parks without another
  provider call. Cross-layer integrated checks remain part of T4.
- T3: complete. Eighteen focused route/claim/replacement/controller cases passed
  (17.488s), real Telegram ownership/durable delivery passed (1.618s), a new
  retirement-publication/failure regression passed (1.137s), and final focused
  Clear/replacement checks passed (1.827s). Daemon owns no route/cache locks,
  maps or event bus; complete replacement stays behind the route owner.
- T4: source and flow audit completed in
  [docs/refactoring-audit.md](docs/refactoring-audit.md). Cold specification
  review and two independent code reviews are clean after one budget handoff
  correction. Focused cross-layer scenarios passed. Static CI stages passed;
  the first managed environment blocked listener and privileged tests. An
  unrestricted rerun passed full local tests and canonical CI after correcting
  a migrated exact-cutoff test fixture. Final `make all` passed before handoff.
