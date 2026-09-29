# Core refactoring completion audit

Scope: [the core goal](refactoring-goal.md), from baseline `7678d2c` through
the current worktree on `refactor/runtime-ownership`. This is evidence for
completion, not a replacement architecture document.

**Status: session boundary corrected; full verification remains open.** The
linked [correction plan](../plan-2026-09-28-session-boundary-correction.md)
removed the temporary live transcript facade and duplicate daemon call scan.
`sessioncalls` now owns call identity and settlement for live execution and
recovery. Independent review found a budget-precedence defect and a nil-response
admission panic in the first implementation; both were corrected with focused
regressions. The full local suite cannot pass in this managed environment because
unrelated listener tests receive `socket: operation not permitted`. The core
goal is not marked complete until the final gates and PR checks pass.
Commit `1dd20e2` gives checkpoint request, input and focus their own control
lock; a concurrent request survives the active checkpoint and a failed terminal
write can be retried without summarizing again while activation continues.
The fired-budget `/compact` terminal-write failure now has a durable
admission/stop design in [ADR-0071](adr/0071-budget-park-settles-accepted-control-inputs.md):
parking settles accepted commands behind the stopping fence, and startup
reserves interrupted parks for that settlement. Focused temporal checks pass;
full correction-phase gates remain open.

## Requirement audit

| Goal requirement | Evidence required | Current conclusion |
| --- | --- | --- |
| No unresolved concentration of unrelated mutable core state | Inspect daemon/session/lifecycle/persistence owners and their consumers; justify retained coordination | The call facade is gone. Session still coordinates activation, transcript, model, tool turns and checkpoints; direct loop accesses are audited below. Final gate evidence remains open |
| Named owners and explicit ordering for representative flows | Trace production entry, state transition, durable boundary and recovery for every flow below | `sessioncalls` owns call identity and settlement; admission and startup scans precede any execution or recovery write. Budget precedence is covered by a full-run SQLite regression |
| Every extraction removes obligations from its former owner | Source search for removed state/mutation paths; compare complete operations before/after | Five forwarding methods, temporary `transcriptSession` and daemon's independent stored-call parser are removed. Live `svc` embeds one call owner; recovery opens it without a runnable session |
| Behavioral checks, architecture sync and independent code review | Exact final commands/results plus clean specification and code reviews | Focused correction tests and `make arch` pass; independent review findings were fixed. Full local suite is environment-blocked; lint, final gate and PR CI remain to be recorded |
| Remaining debt has explicit reasons | Distinguish necessary coordination, optional organization and unresolved defects | Session size and direct loop access remain; retained activation coordination is justified below. No schema or status was added for call ambiguity |

## Integrated flow evidence

| Flow | Authority and ordering to verify | Existing evidence anchors |
| --- | --- | --- |
| Accepted input to response | Daemon admission routes durable input; inputruntime promotes it; session decides an accepted-response disposition; sessionstore atomically records message/accounting/budget/candidate/output | Durable loop/input-order tests; response-disposition tests; input-recovery and response-integrity scenarios |
| Child completion to parent | Lifecycle derives the canonical recovered outcome; subagent transactions arbitrate activation and delivery; parent reload follows a winning transcript/link commit; typed routing keeps blocking results distinct from background inbox facts | Completion/rearm/restart protocol models; candidate-result and duplicate-delivery scenarios |
| Stop and interrupted-stop recovery | Tree fence and durable stopping intent precede producer cancellation; signal all runners before joins; cancellation Done precedes fence-dependent child finalization; coordinator settles unambiguous calls before terminal stop output | Supervisor protocol tests; cancellation/fence regression; interrupted-stop, orphan-settlement and ambiguous-call scenarios |
| Budget crossing | Response accounting and cost crossing stay one transaction; independent lifecycle timer observes duration; generation-fenced park joins work and uses the same tree-stop operation | Budget transaction tests; independent deadline/restart/shutdown tests; park race and budget scenarios |
| Context checkpoint | Safe point follows completed tool batches; checkpoint owner preserves candidate/nudge and row identities; atomic replacement precedes baseline invalidation and outcomes; publication failure cannot undo a committed result | Compaction protocol model; pin, native-prefix, ordering and command scenarios; new post-commit progress-failure regressions |
| Model lifetime | Owner leases the current client through Chat; switch replaces identity and measurement generation together; terminal close rejects and releases a late replacement | In-flight switch/close tests; constructor-barrier regression; same-model baseline restore and reset/compaction invalidation |
| Resource shutdown | Daemon closes admission and cancels recovery; joins runners/processes/reconcilers/park workers; session closes model before stack; shared tool-resource lifetime closes last | Daemon shutdown/recovery tests; composition-root lifecycle tests; factory-wrapper resource/process regression |
| Manager ownership and delivery | Route owner serializes claims and replacement; stale route reads yield to committed claims; replacement retains its ownership fence through old-root retirement; durable outbox remains delivery authority | Publication ownership model; concurrent-claim and late-claim tests; replacement routing and real Telegram delivery scenarios |

## Responsibility changes

| Boundary | Former distributed responsibility | Inspected result |
| --- | --- | --- |
| Lifecycle supervisor | Daemon mutated registry, admission counters, queues and tree locks separately | Those containers exist only under supervisor; daemon invokes complete operations |
| Accepted response and recovered outcome | Optional dependencies selected a legacy response algorithm; lifecycle reinterpreted transcript meaning | One executable disposition path; one store projection for recovered outcome |
| Budget reconciliation | Progress capture also enforced deadlines and triggered park | Progress has no budget-control dependency; independent worker is cancellable and joined |
| Model runtime | Session held client/configuration/mutex/epoch/baseline | Model owner contains all state and mutations; consumers use leased operations and snapshots |
| External-call coordinator | Daemon staged calls, claimed/applied config and settled grants/transcripts across unrelated runner methods | Daemon owns no staged/applier/activation-store fields; coordinator retains producer claims and lifecycle ordering while delegating transcript identity/settlement to `sessioncalls` |
| Tool-call identity and settlement | Live `svc` forwarded through a temporary `transcriptSession`; daemon parsed stored calls with a second unresolved-call algorithm | One stable live call owner and one store-backed recovery owner share a scanner. Duplicate IDs block execution and settlement without a new status or schema |
| Tool-turn owner | Tool pipeline accepted the complete mutable session and updated detector/grant/suspension/progress directly | Pipeline accepts direct capabilities and operation inputs; typed outcomes carry committed effects even on later errors |
| Checkpoint owner | Session and loop shared command/focus/defer/attempt/summary state and checkpoint transactions | One owner performs complete attempt/command lifecycle; loop only selects safe point and adopts outcome |
| Manager-route owner | Daemon held publication caches and claim/replacement serialization | Route state and mutations reside together; replacement has one complete ownership-fenced operation |

## Integrated ownership map

Arrows indicate coordination or a direct capability dependency, not package
imports. Private owners remain inside their existing packages.

```mermaid
flowchart TD
    Root[Composition root] --> Daemon[Daemon coordination]
    Daemon --> Lifecycle[Lifecycle supervisor]
    Daemon --> Calls[External-call coordinator]
    Calls --> CallIdentity[Tool-call owner]
    Daemon --> Routes[Manager-route owner]
    Daemon --> Session[Session activation loop]
    Session --> CallIdentity
    Session --> Inputs[Input runtime]
    Session --> Model[Model runtime]
    Session --> Turns[Tool-turn owner]
    Session --> Checkpoint[Checkpoint owner]
    Turns --> Transcript[Transcript projection]
    Checkpoint --> Transcript
    Checkpoint --> Model
    CallIdentity --> Transcript
    Inputs --> Transactions[Complete SQLite transactions]
    Transcript --> Transactions
    Session --> Transactions
    Lifecycle --> Child[Subagent activation and delivery transactions]
    Lifecycle --> Budget[Independent budget reconciler]
    Routes --> Outbox[Manager ownership and durable outbox]
    Transactions --> Outbox
```

Source anchors for the map and ordering:

- [Supervisor](../internal/sessionlifecycle/supervisor.go) pairs admission and
  registry lifetime; [tree stop](../internal/sessionlifecycle/tree_stop.go)
  owns signal-all, join, resource retirement and settlement ordering.
- [Child completion](../internal/sessionlifecycle/completions.go) terminalizes
  through the subagent transaction, reloads a parent only after winning
  delivery, then attempts rearm. Callbacks route effects across owners.
- [External-call settlement](../internal/daemon/external_call_settlement.go)
  adopts orphan identity before opening a transcript and releases temporary
  adoption even on failure. Ordinary result ownership retires after acceptance
  in [Resolve](../internal/daemon/external_calls.go).
- [Shared call identity](../internal/sessioncalls/scan.go) rejects repeated IDs
  before execution or recovery settlement; the [store-backed owner](../internal/sessioncalls/stored.go)
  preserves atomic result replay without creating a runnable session.
- [Manager routes](../internal/daemon/manager_routes.go) retain ownership
  serialization through replacement and retirement; publication uses only the
  independent cache lock, so retirement can publish within the fence.
- [Model runtime](../internal/session/model.go) leases the client through I/O
  and makes close terminal. [Tool turns](../internal/session/toolexec.go)
  consume explicit grants and return committed effects; they receive no
  session container. [Checkpoints](../internal/session/context_runtime.go)
  own command/defer/attempt state; the loop chooses the safe point.
- [Accepted-response persistence](../internal/sessionstore/completion_check_store.go)
  commits paid message, common budget observation, disposition and output in
  one transaction. [Tool-result persistence](../internal/sessionstore/direct_output_store.go)
  owns call identity, completion invalidation and optional direct output.

The remaining session transcript lock intentionally protects one projection and
its row-ID sidecar. Checkpoint operations use that same projection; model
operations never acquire its lock. A second transcript container would make
ordering and replacement harder to prove. The live call owner reads that same
projection through narrow functions and owns no second transcript state.

## Retained coordination and deferred work

- **Daemon startup/shutdown and producer routing:** the application must order
  stop recovery, process interruption, call settlement and ordinary resume. It
  also selects delivery variants across genuine owners. Retain this coordination
  only where it consumes complete domain operations rather than mutating their
  private state; independent review confirmed that condition.
- **Session loop and activation protocol:** input acquisition, safe operation
  points, terminal grant resolution and budget-driven exit belong to the one
  activation. Tool turns consume an activation but do not own every transition
  that can occur without a tool call.
- **Shared SQL receiver:** it owns only a database handle. Consumers already
  receive narrow capabilities; response, input, output and child-delivery
  invariants require cross-table transactions. Splitting by table would not
  reduce an established state owner and is deferred absent a concrete problem.
- **Project-store location, method/file counts and query organization:** useful
  only when tied to an independently changing responsibility or demonstrated
  performance problem. They are not size quotas for this effort.
- **Telegram manager decomposition:** outside the core goal. Manager ownership
  and durable delivery behavior remain covered by integration evidence.
- **Budget gate scheduling callback:** `sessionBudgetGate` still references the
  daemon for its park effect. It holds no competing mutable budget state; the
  budget transaction owns firing and the daemon owns tree parking. A narrower
  callback would reduce a dependency edge but would not remove another owner.
- **Compaction deferral announcement:** this small cache spans reconstructed
  session objects and belongs to daemon identity coordination. Moving it into
  one session object would lose its lifetime guarantee.

## Audit findings and closure evidence

- The session-boundary correction reduced `internal/session` production code
  from the 9,237-line plan baseline to 9,088 lines, but added the 451-line
  `sessioncalls` package and one explicit dependency edge (20 to 21). Those
  counts do not prove improvement. The before/after change is ownership: live
  `svc` no longer manufactures a five-method transcript facade, and daemon no
  longer decodes and deduplicates stored calls separately. One scanner now
  rejects overlapping unresolved IDs in both paths while permitting a later
  invocation to reuse an ID once its predecessor has a result.
- The loop still has about 100 direct `r.agent` references. Its largest groups
  are transcript (`ms`, 17), input/output boundary (15), and activation grant
  (`currentActivation`, 11); these participate in the one activation's safe
  point and terminal ordering. Model, tool-turn and checkpoint state remain in
  their named owners. The retained `svc` methods compose and expose those
  capabilities; another extraction would need to move a complete transition,
  not just these references. Checkpoint control no longer shares the transcript
  lock. The live call owner has no independent message or row-ID container;
  recovery caches a read-only snapshot for its context-free projections and
  reloads durable rows for every exact settlement.
- Daemon still owns startup/shutdown ordering, producer routing and stop fences;
  `sessionlifecycle` owns runners and tree joins; `sessionstore` owns the atomic
  accepted-response and result transactions. The corrected call owner neither
  recreates these ledgers nor crosses their authority. No additional unrelated
  mutable protocol was identified in this audit; package size alone remains
  deferred as described above.
- A fresh overlapping call or repeated ID in one model response executes no
  tools. Its exact paid attempt is retained outside active context and the host
  requests a bounded retry; a fired budget remains the sole terminal output.
  Result replay and direct output use the saved assistant row and call index,
  not a session-wide provider ID. Migration 46 preserves legacy rows and
  refuses to guess an ambiguous old result owner. Focused SQLite, scanner and
  compaction/reuse regressions cover the corrected protocol. Independent review
  previously closed a budget-output precedence defect and a nil-response
  admission panic.
- The old InsertBudgetedResponse algorithm had no production callers, but its
  tests hid missing budget observation/non-execution in canonical dispositions.
  It was removed and the canonical transaction corrected. The migrated store
  scenarios failed before correction, then passed with independent cost-model,
  disposition-matrix and rollback assertions (10.207s). A daemon scenario
  reproduced two tool executions after crossing, then passed with zero crossing
  executions and correct park/resume behavior (0.786s); the same real daemon
  scenario passed again after the post-commit adoption fix (1.196s).
- Two more test-only alternate transactions, InsertAssistantMessageWithOutput
  and InsertToolResultWithDirectOutput, were removed. Their atomic output,
  generation, stop and replay tests now use canonical operations and passed
  focused acceptance; ordinary generation seed corpus passed too. Host final
  promotion, commands and lifecycle output remain distinct legitimate operations.
- Final ownership inspection found two residual contract gaps: nil-output
  recovery selected raw result insertion without canonical replay/invalidation,
  and heartbeat cancellation did not join its worker. The validated
  [correction plan](../plan-2026-09-28-settlement-and-worker-lifetime.md) also
  preserves attachments omitted by the canonical insert. Both corrections and
  their real-SQLite/deterministic lifetime regressions passed focused acceptance.
- Independent code review found one consequential budget handoff bug after the
  canonical transaction: a failed transcript reload hid its already-committed
  fired verdict. The new real-SQLite test failed before the fix and passed after
  it; the session now schedules the park before reading history and leaves the
  committed response suspended on a read failure. Both code and specification
  re-reviews found the correction sound.
- A cold review of the complete branch found a further P2 budget-recovery
  regression: after a transient park failure, the new reconciler scanned only
  armed budgets, so a fired generation stayed requested/draining until restart.
  It now scans pending parks on each live tick and retries after one second;
  daemon workers deduplicate concurrent attempts for one root/generation and
  release that slot after completion or panic. Real-SQLite requested/draining
  retries and deterministic worker deduplication passed. Code and specification
  re-reviews found no remaining consequential mismatch.
- A focused full-CI failure in `TestHeaderCheckCountsTheSystemPrompt` was a
  fixture wiring error: the test replaced the session prompt pointer after the
  checkpoint owner had captured the earlier pointer. It now reconstructs the
  owner with the new prompt; the exact header checks passed.
- The unrestricted CI run exposed the same fixture-wiring error in
  `TestShouldCompact/at_cutoff_exactly_is_not_over`. Reconstructing its
  checkpoint owner with the replacement prompt restored the exact-cutoff
  scenario. The focused test and complete CI rerun passed.
- Semgrep's old `runner.go`-only call-settlement exception was updated for the
  three exact external-call owner files. Independent review confirmed their
  queued-result, fenced-abandonment and startup-orphan paths and found no
  bypass; the full Semgrep scan passed afterward.

## Session-boundary verification record

- Focused scanner, live response, budget crossing, grant admission, stored
  settlement, stop and runner-admission tests passed against real SQLite where
  durable behavior matters. The full-run budget regression asserts one
  checkpoint outbox row and `suspended` status, with no tool execution.
- Independent code review found the budget second-output and nil-response
  admission defects; both were corrected. A separate lint review and full
  `make lint` passed with zero issues. `make arch` passed after the package-map
  update; `make all` again passed format, build, lint and architecture stages.
- Full local `make test` and final `make all` exited 2 when unrelated packages
  tried to bind TCP or Unix listeners (`socket: operation not permitted`). The
  delegated local `CI=true make ci` passed format, build, lint and architecture,
  then stopped before Semgrep because `uv` could not write its tool directory
  under this managed sandbox. Semgrep, secrets, integration tests and PR CI
  remain unverified at this checkpoint.

## Prior checkpoint verification record

- Cold specification review of the integrated five plans: clean, with no
  concrete implementation-to-plan mismatch. All committed session test and
  literal subtest names remain after fixture recovery; exact unsaved local
  assertions cannot be independently compared.
- Independent code review of the complete refactor: one P1 committed-budget
  adoption finding corrected and re-reviewed clean; the complementary daemon,
  lifecycle and routing review was clean. A later complete-branch review found
  the P2 live-park retry regression above; it was corrected and re-reviewed
  clean. The independent durability review found no P0–P2 defect.
- Full local `make test`: exit 0 in the unrestricted environment. All fast
  packages passed, including the listener-dependent packages previously blocked
  by the managed sandbox. The delegated full `make lint` also passed with zero
  issues after the final fixture correction.
- Canonical `CI=true make ci`: the first unrestricted run passed format, build,
  lint, architecture, Semgrep and both gitleaks scans, then found the exact
  compaction-cutoff fixture failure above. The focused correction passed, and
  the complete rerun exited 0 with every tagged integration package passing,
  including daemon, session, sessionstore, Telegram and migrations. Fixture Git
  configuration was isolated from the invoking user's credentials.
- After the complete-branch review correction, full local `make test` and
  canonical `CI=true make ci` passed again, including daemon, session and
  sessionstore integration suites.
- Final `make all`: exit 0 after the branch-review correction and documentation
  update; formatting, build, lint, architecture and bounded local tests passed.
- Final source/flow audit and architecture reconciliation: complete; the
  source anchors above match the synced ARCHITECTURE.md, glossary and ADRs.

The lost pre-cleanup local test assertions cannot be compared byte-for-byte
without a snapshot. Every committed test and literal subtest name was restored;
the selected recovery scenarios and complete local/CI suites passed. This is a
coverage provenance limit, not a failing gate.
