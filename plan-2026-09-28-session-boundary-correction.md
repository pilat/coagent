# Session boundary correction

## Goal

Finish the session ownership work where the first refactor left false boundaries:
the live session must not construct a fresh transcript facade to forward its
public methods, daemon and session must use one definition of unresolved calls,
and checkpoint control state must not borrow the transcript lock. Preserve
restart, stop, completion, compaction and durable-result behavior. The package's
9,237 production lines and 20 internal dependencies are baseline measurements,
not a success threshold; removing obligations and dependency edges is the goal.

This plan advances [the core goal](docs/refactoring-goal.md). The current goal
checkpoint remains open until this correction and another ownership audit pass.

## Decisions

1. Keep one `svc` as the composition-root-wired live `session.Service`. The
   naming rule in `docs/coding-style.md` applies to that component, not every
   private helper. A transcript-only recovery capability remains necessary:
   daemon must settle orphaned and interrupted calls without constructing a
   model client, tool registry or executable session.
2. Put external-call classification and settlement in a dedicated
   `internal/sessioncalls` component. It owns no agent loop, model or checkpoint
   state. Live execution supplies its existing `messageStore` through a narrow
   result/snapshot capability. Recovery opens a store-backed call owner directly
   in `sessioncalls`, using the same scanner and result transaction; remove
   `session.OpenTranscript` and its temporary `messageStore`. This keeps
   recovery free of model/tool resources and removes a second decoder.
   The live session retains one stable call owner for its lifetime instead of
   creating an adapter for each method. `session` and `daemon` use the same
   whole-transcript classification; current-turn interrupted-call semantics stay
   distinct and explicit. A stored-row decoder may adapt durable messages to
   the shared classifier but may not make separate unresolved-call decisions.
   Do not move all of `messageStore` into this component:
   it also owns compaction transactions, and exporting its internals now would
   create a wider boundary than the one being removed.
3. Give checkpoint command/request/focus state its own synchronization. The
   transcript mutex protects messages and row IDs only. A safe-point claim
   atomically takes one pending request, its immutable command input and its
   focus into a separate active attempt. A request arriving during that attempt
   remains queued and cannot change the active summary or be cleared by its
   terminal path. If publication or durable settlement fails, the active input
   remains retryable; clear it only after its durable terminal outcome. Keep
   the candidate/row-ID replacement transaction and its existing ordering.
   A fired-budget command uses the park path; [ADR-0071](docs/adr/0071-budget-park-settles-accepted-control-inputs.md)
   now settles its durable terminal output behind the stop admission fence.
4. Provider call IDs identify model-visible call/result pairs, not all results
   ever stored in a session. The shared scanner rejects an overlapping pending
   ID or two equal IDs in one response, but permits reuse after the earlier
   invocation is answered. An unusable fresh response is paid evidence outside
   active context, with bounded retry and no tool execution. Durable replay is
   keyed by the saved assistant row and call index; migration 46 adds nullable
   owner fields while preserving older results. Ambiguous legacy ownership
   fails closed rather than being guessed or silently rewritten.
5. Do not replace `loopRunner.agent *svc` with one large interface. Audit its
   direct accesses after tasks 1–2 and extract only a complete transition if
   it removes direct mutation; otherwise document why activation coordination
   remains in `session`. The package may remain large if its responsibilities
   are cohesive, but no completed-goal claim can rest on moved lines alone.

## Constraints

- Preserve accepted-input durability, exact child-round identity, stop fences,
  budget crossing, candidate confirmation, atomic result replay, attachment
  identity, compaction row IDs and manager delivery ordering.
- `InsertToolResultSetOnce` remains the durable result path even when output is
  disabled; no raw-message fallback and no duplicate executable algorithm.
- Keep the modular monolith and current dependency tiers. Add only the explicit
  imports needed for `sessioncalls` in `.go-arch-lint.yml`; it must not import
  `session` or `daemon`. Avoid an event bus or generic state machine.
- Existing migrations and product/provider/sandbox behavior remain unchanged;
  a new migration may add durable call ownership without deleting history.
  Telegram decomposition and unrelated ADR reorganization are separate work.
- Preserve existing temporal scenario names and assertions when fixtures move.
  Read `docs/testing.md`; run focused checks at checkpoints, then the repository
  handoff gates and independent review. Do not run mutation or CI-only targets
  directly from the implementation session.

## Affected code

- `internal/session/{transcript_session,external_call,message_store,context_runtime,loop_context,compaction,serialize}.go`
  and direct callers/fixtures: call ownership, transcript projection access,
  checkpoint control and safe-point claim.
- `internal/daemon/{external_call_transcript,external_call_settlement,external_calls,runner,completion}.go`
  and relevant protocol tests: replace the duplicate stored-call scanner and
  open the store-backed call capability directly for recovery.
- New `internal/sessioncalls/`: one call-settlement implementation and one
  canonical whole-transcript scanner, with explicit current-turn operations.
- `.go-arch-lint.yml`, `ARCHITECTURE.md`, `docs/glossary.md` and the linked
  refactoring audit/goal checkpoint: record the actual new package boundary and
  corrected completion claim. A hard-to-reverse package decision needs an ADR.

## Tasks and acceptance

### T1: Checkpoint control ownership

Move `pendingCompaction`, `compactionFocus` and `compactionInput` under a
checkpoint-owned lock or equivalent single atomic state. Define the safe-point
claim as an immutable input+focus snapshot, separate from later queued state,
so a concurrent new request is not cleared by the in-flight attempt. Retain an
active command through a failed start notice or terminal write while activation
continues; a committed result must not run the model twice while publication is retried.
Keep transcript locking around candidate creation and atomic replacement, and
do not hold the control lock across model I/O or publication.

Acceptance: no checkpoint-only field is guarded by `messageStore.mu`; a request
arriving during an attempt runs at a later safe point; explicit `/compact`
settles once on durable success or error while the activation continues.
The fired-budget terminal-write failure is addressed by ADR-0071. Run exact
command/deferral/commit, candidate-pin, reset and post-commit publication-failure tests, plus a new
deterministic concurrent-request trace with real SQLite state.

### T2: One external-call authority

Move the durable call vocabulary, whole-transcript unresolved-call scanner and
settlement operations to `sessioncalls`. Reject repeated call IDs in the
same canonical scan for both daemon and live session; preserve the separate
latest-assistant-turn rule for interrupted in-loop calls. Make session's live
call owner persistent, remove the per-method temporary `transcriptSession`
facade, `session.OpenTranscript` and daemon's independent stored-call parser.
Recovery opens the same component without a model or tools. Preserve in-memory
reload after a winning durable commit and keep compaction's already-locked call check free of
reentrant locking.

Acceptance: one production unresolved-call algorithm is used by live and
recovery paths; no five-method forwarding layer or second public service-like
type remains in `internal/session`; daemon recovery never constructs a full
session; pending overlap fails before execution, while reuse after completion
gets a new durable result. Run exact
external-call, orphan/restart, interrupted-stop, replay/attachment and
compaction-pending-call scenarios, including a new duplicate-ID parity test and
an interrupted-recovery trace that rejects an overlapping ID without partially
inserting results for earlier calls, plus replay after compaction and ID reuse.

### T3: Integrated ownership audit and handoff

Recount `svc` obligations, package imports, direct `loopRunner.agent` accesses
and cross-owner locks. Record the before/after dependency and state-access
changes, justify retained activation coordination and any remaining large type.
If the audit finds another unrelated mutable protocol still owned by `svc`,
resolve that concrete protocol before marking the core goal complete; do not
declare success from the new package's existence or a line-count drop.

Sync architecture docs, write the package-boundary ADR, run focused temporal
checks, full `make test`, independent implementation-to-plan and code reviews,
delegated lint and canonical CI, then final `make all`. Commit the coherent
correction as a new Conventional Commit and update the existing draft PR;
monitor its checks to a terminal state.

## Out of scope, accepted

- Full package-per-type decomposition: model, tool turn and checkpoint owners
  already hold distinct state; moving them before narrow contracts exist would
  export private internals and preserve the same coupling elsewhere.
- Arbitrary file/line-count quotas: package size is a warning signal, not an
  ownership invariant. The audit must explain each retained dependency instead.
- ADR renumbering: stable decision identifiers and historical references are
  a separate documentation task; it does not repair `session` ownership.
