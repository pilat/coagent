# Durable two-phase model completion

## Goal

Prevent a naturally stopped model response from ending unattended work before
the model has deliberately checked that it is done, without bringing back
model-authored waiting markers or requiring a terminal tool call. A normal
`stop` remains an immediate activation yield when a durable background process
or subagent will wake the session. Otherwise the loop gives every root and
subagent one durable, host-authored second look, keeps the first answer hidden
from the human, and accepts the next non-empty `stop`. The protocol must survive
daemon restart and abrupt process loss without duplicate nudges, leaked
candidates, repeated tool side effects, or an unbounded empty-response loop.

## Decisions

1. **A model finish reason describes one attempt, not task completion.** Keep
   provider-native finish evidence and `llmwire.FinishType` normalization from
   ADR-0052. `stop` / `end_turn` proves only that generation ended normally.
   Session-loop policy decides whether that accepted response ends the current
   activation.
2. **Completion uses one agent-type-independent state machine.** Apply the same
   rules to roots and every subagent type. Do not put completion behavior in an
   agent prompt, a root-only daemon branch, or a particular manager. Subagents
   with no todo list receive the same generic second look as a root with no todo
   list.
3. **Response-integrity routing keeps precedence.** Reject `length` and
   `unknown` exactly as ADR-0052 requires before completion policy runs. Actual
   tool calls continue to route structurally even when the provider reports
   `stop`. A `tool_calls` finish with no calls remains an empty-response case;
   it is not task completion.
4. **A durable background wake source bypasses completion checking.** For a
   normalized `stop` with no tool calls, first project whether the exact session
   owns an advertised running Bash process, an undelivered non-blocking child
   link in `spawned`, `running`, `completed`, or `error`, or pending
   `source=process|subagent` inbox input. `stopped` and `killed` child links are
   not wake sources: neither promises automatic delivery. If a source exists,
   trust the
   `stop`, publish its non-empty text through the existing output path, complete
   the activation, release the runner, and wait for ordinary durable completion
   input. An empty `stop` on this path also yields immediately and neither
   increments empty-loop state nor creates a nudge.
5. **Wake projection reuses the ledger-first race closure.** Factor the existing
   `hasBackgroundObligation` definition so root-budget retention can query a
   subtree while completion policy queries one exact session. Read advertised
   running process rows and undelivered non-blocking child links before pending
   async inbox rows. Producer terminalization/delivery commits its ledger change
   and inbox row together, so the ordered reads see either the earlier live
   obligation or the later queued completion. A projection error never guesses
   that no wake source exists: atomically retain the paid assistant attempt and
   usage as hidden evidence, apply budget precedence, and otherwise commit the
   session's existing durable error outcome plus its host notice. Mark that
   terminal state committed so generic error persistence cannot duplicate it.
6. **No-wake non-empty stops are two-phase.** When no completion check is
   pending, persist the assistant response as a final candidate, append one
   host-authored user-role completion nudge, keep the session active, and make no
   manager output. The candidate remains in the provider transcript so the
   model can review what it proposed, but it is not sent through PubSub or
   `session_outbox`. When a completion check is pending, the next accepted
   non-empty no-tool `stop` is deliberate confirmation: publish only this second
   response and finish normally, even if todo items remain open.
   While the check is pending, compaction must pin the exact candidate+nudge pair
   in the verbatim tail. Older history may still compact; if no legal checkpoint
   can retain the pair, use the existing non-relieving/header-too-large behavior
   rather than summarize away the evidence being confirmed.
7. **The confirmation is state, not syntax.** Add nullable durable session state
   `completion_check_candidate_id` pointing to the exact candidate assistant
   message. Do not use a boolean, an exact `OK`, a
   prompt substring, `retry_of_message_id`, or another assistant-authored
   marker. The message identity makes stale and replayed transitions
   detectable. A live `session.svc` may cache the value loaded through
   `SessionRecord` / `CreateOptions`, but SQLite is authoritative.
8. **Every external model-visible input invalidates an old check.** In the same
   transaction that makes a non-host input model-visible, clear the candidate
   identity and reset the empty-stop streak. This includes user and agent inbox
   promotion, process and subagent completion promotion, scheduled/internal
   synthetic deliveries, direct external-call results, blocking-subagent result
   insertion, and context replacement. The completion nudge is the sole
   exception. Give that insertion a typed store/session path; never recognize it
   by its English text. Like ADR-0052 recovery input, the host nudge continues
   the same external turn and does not advance `model_input_generation`.
9. **A tool-bearing model response invalidates the check at response commit.**
   Clear the candidate identity and empty-stop streak atomically with persistence
   of an accepted assistant response containing tool calls, before execution.
   This ensures a crash cannot retain confirmation state beside a durable tool
   request. ADR-0059 still prevents an interrupted in-loop call from being
   re-executed after restart; after its typed failure, the next no-tool `stop`
   starts a fresh completion check. Clearing the candidate does not clear an
   outstanding manager reply obligation.
10. **The nudge is rendered once in the shared session layer.** Inspect the
    current todo service and include only `pending` and `in_progress` items,
    canonically ordered. With open items, name them and tell the model to
    continue with tools or deliberately stop again with an explanation; it may
    reconcile completed/cancelled/no-longer-applicable items first. With no open
    items, do not mention todo at all: ask it to re-check the original request
    and completed work, continue with tools if action remains, or answer once
    more with a concise explanation of why it is stopping. Exact acknowledgement
    text is never required.
11. **Hidden checking preserves the original reply obligation.** The first
    candidate neither releases nor answers the accepted manager input. Carry
    a separate durable `manager_reply_pending` session flag, rather than storing
    it inside completion-candidate state or reconstructing a process-local flag
    from old inbox rows. Set it when a manager-owned model input is promoted;
    preserve it across candidate invalidation, tool calls, restart, and unrelated
    async input; clear it only in the same transaction that commits a releasing
    final/error/budget output or a terminal lifecycle output that supersedes the
    owed model reply. Read-only command receipts such as `/status` do not clear
    it. A confirmed root response is therefore still a
    persistent, releasing answer to the manager; autonomous turns with no pending
    manager reply retain their existing replaceable/releasing output behavior.
    Subagents never set the flag and emit no manager output; only the confirmed
    response becomes their parent-facing result. `loopRunner.replyToInput` may
    remain a cache, but it is initialized and reconciled from this durable fact.
12. **Empty responses are a durable anti-loop signal, not confirmation.** Move
    the existing empty-response count under the loop detector as a distinct
    `empty_stop`/no-progress event rather than a fake tool record. Persist the
    trailing streak on the session. A whitespace-only no-tool response without
    a wake source increments it; the third response receives the current strong
    warning and the sixth commits one durable host notice and ends the activation
    through ordinary successful completion. An empty response never sets or
    clears a pending completion candidate. A non-empty accepted response, a
    tool-bearing response, or external model-visible input resets the streak;
    host-authored empty/completion nudges do not. The sixth-response notice must
    reach a root manager or become the result delivered for a subagent, so no
    agent type silently completes with an empty result. For a child, the durable
    terminal streak is the recovery evidence: `sessionlifecycle` derives the
    shared host notice as a successful child result when the streak is six,
    instead of fabricating a model-authored assistant answer or returning its
    generic incomplete outcome.
13. **Each causal transition is one SQLite commit.** Extend the accepted-response
    persistence boundary, following `CommitRejectedResponse`, so assistant
    message insertion, iteration advancement, completion-check set/clear,
    empty-streak update, host nudge, budget verdict, and optional manager output
    have one disposition. Do not append a response and mutate a process-local or
    session-row flag afterward. Return committed message identities/outcome to
    the message store and update its in-memory projection only after commit.
    Final rendering also belongs to this boundary: after applying the response,
    iteration, usage, todo and budget changes inside the transaction, capture
    the post-disposition progress facts on that transaction and pass them to a
    pure, database-free final renderer before inserting the outbox row. Do not
    call the current store-reading renderer before or after the commit.
14. **Restart replays obligations, not decisions.** A crash before the response
    disposition commit may repeat the provider call and its cost because no
    result was durable. A crash after candidate+nudge commit resumes the one owed
    confirmation without adding another nudge. A crash after confirmed final or
    background yield observes the accepted assistant/output fact and completes
    session status without another model call. A crash after a tool-bearing
    response observes cleared confirmation state and follows ADR-0059. A pending
    empty streak resumes at its durable count rather than restarting at zero.
15. **Budget policy remains stronger than automatic recovery.** Observe the
    root-tree budget in the same response transaction. A budget crossing on the
    first candidate suppresses the completion nudge and publishes only the host
    budget checkpoint; it must not expose the unconfirmed candidate text. A
    budget crossing on a confirmed response may retain the confirmed text under
    the existing budget output contract. Admission firing between candidate and
    confirmation parks the tree; the next external model-bound input both
    releases/resumes according to existing budget policy and invalidates the old
    completion check.
16. **Lifecycle vocabulary does not grow.** A confirmed stop, background yield,
    or sixth empty response ends with the existing successful `completed`
    session outcome. Do not add `incomplete`, `blocked`, `waiting`, or another
    status. Open todo state is evidence shown to the model, not a lifecycle veto
    after the second non-empty stop.
17. **The decision supersedes ADR-0053 without resurrecting its rejected
    protocols.** Carry forward ordinary background handoff and durable inbox
    wake, but replace “every no-tool stop immediately completes” with the
    wake-aware two-phase rule. ADR-0052 remains accepted because its attempt
    integrity and finish evidence are prerequisites, not competing task-complete
    semantics.

## Constraints

- Do not add a final-answer tool, waiting tool, forced tool choice, exact-text
  marker, `OK` token, polling timer, or model-authored control syntax.
- Do not require a model to call a tool merely to yield while background work
  owns a wake-up.
- Never expose the first no-wake final candidate through manager output,
  progress replacement, direct notification, subagent completion, budget text,
  or a max-iteration fallback. If the 1000-iteration defect breaker fires while
  a check is pending, emit its host error/notice without promoting candidate
  text.
- Keep accepted candidate text in the provider transcript; it is hidden from
  the human, not rejected provider content. Do not overload
  `messages.rejected_reason` or the ADR-0052 `retry_of_message_id` relation.
- Keep `completion_check_candidate_id`, `manager_reply_pending`, and the empty
  streak durable. A plain field on `loopRunner`, `session.svc`, or the daemon is
  not an authority.
- Preserve append-only messages, tool-call/result pairing, manager output
  generation stamping, accepted-input release semantics, subagent activation
  sequencing, and the 1000-iteration defect breaker.
- A final footer must be rendered from the transaction's post-response facts,
  including the current iteration, usage and budget verdict. Rendering must be
  pure while the transaction is open; it may not perform nested store reads,
  external I/O, notifications, or progress reconciliation.
- Preserve the exact candidate+nudge pair across automatic and explicit
  compaction while its candidate identity is pending.
- Keep response-integrity behavior for `length` and `unknown`, including its
  linked retry and budget precedence. A rejected attempt does not satisfy or
  reset a pending completion check.
- Preserve `/stop`, `/kill`, `sleep`, blocking external calls, configuration
  restart, and background completion suppression behavior.
- Migration is forward-only. Add the next free migration at implementation time
  (currently `00040`; another queued change may claim that number first) and do
  not edit an existing migration.
- Completion-check and empty-loop behavior must be deterministic under daemon
  restart, input races, output redelivery, and subagent reuse. Use migrated real
  SQLite in protocol/restart tests; do not rely on SQL mocks or sleeps.
- Follow `docs/testing.md`: preserve the observed conversation ordering in a
  sanitized scenario fixture and assert controller-visible output after final
  rendering. Locally run exact tests for CI-owned packages, not their complete
  suites.
- Preserve unrelated dirty-worktree files. Do not stage, commit, push, or open a
  PR unless separately requested.

## Out of scope, accepted

- **A separate evaluator model.** The same agent performs the second look; a
  second model would add routing, cost, and failure semantics not needed for the
  agreed safeguard.
- **A semantic proof that the task is complete.** The second non-empty stop is a
  deliberate confirmation, not formal verification. Deterministic todo evidence
  strengthens the prompt but cannot prove external work quality.
- **A new incomplete/blocked session status.** Existing callers understand only
  the current lifecycle; the model's final explanation carries any limitation.
- **Configurable completion or empty-loop thresholds.** The completion check is
  exactly one second look; empty responses keep the existing warning-at-three
  and finish-at-six behavior.
- **Persisting the existing tool-diversity window.** This change persists the
  new empty-stop streak because it is part of the completion protocol, but does
  not redesign restart semantics for the established tool-loop detector.
- **Preventing duplicate provider billing before durable commit.** If the daemon
  dies after receiving a provider response but before SQLite commits it, the
  accepted input is retried; there is no durable response identity to recover.
- **Changing provider finish mappings.** Anthropic `end_turn`, OpenAI-compatible
  `stop`, and Google natural-stop normalization remain transport concerns.
- **Retroactively challenging historical final responses.** Migrated sessions
  start with no pending completion check and a zero empty streak. Existing
  accepted terminal assistant/output rows keep their recovery behavior.

## Affected code

- `migrations/<next>_durable_completion_check.sql` and
  `internal/migrate/*test.go` — add nullable candidate-message identity, a
  durable manager-reply obligation, and a non-negative durable empty-stop
  streak to `sessions`, with safe defaults for existing rows.
- `internal/sessionstore/store.go` and `transaction_owners.go` — project the new
  session fields and expose a narrow runtime response-disposition transaction.
- A focused `internal/sessionstore/completion_check_store.go` plus tests — own
  atomic candidate/nudge, confirmed/yield output, check reset, empty-streak,
  iteration, and budget composition. Reuse existing output, budget, and message
  insertion fragments instead of creating a second output ledger.
- `internal/sessionstore/inbox_store_helpers.go`,
  `scheduled_delivery_store.go`, and other model-input insertion transactions
  found by tracing `advanceModelInputGeneration`, direct tool-result delivery,
  and context reset — clear completion/empty state atomically for every external
  model-visible input except the typed completion nudge.
- `internal/sessionstore/{stop_completion_store,shield_command_store,state_output_store}.go`
  and lifecycle tests — clear a superseded manager reply obligation atomically
  with terminal `/stop`, shield, and error settlements while preserving it for
  read-only command receipts.
- `internal/subagent/transactions.go` and its tests — ensure blocking completion
  rows reset the parent check in the same cross-table transaction; background
  completions reset it when their inbox row is later promoted.
- `internal/session/{factory,factory_create,session,loop,message_store,budget_gate}.go`
  and a new focused completion-policy file — load the durable projection, render
  the single conditional nudge, route every accepted response through the
  disposition transaction, preserve reply ownership, and adopt committed rows
  only after success.
- `internal/session/loopdetect.go` and focused tests — absorb the empty-response
  event/streak and its 3/6 escalation without mixing it into tool diversity.
- `internal/session/{compaction,compaction_checkpoint,cut}.go` and focused tests
  — constrain checkpoint selection so a pending candidate and its nudge remain
  verbatim until the check resolves.
- `internal/daemon/runner.go`, `budget_tool.go`, and wake-projection tests —
  inject an exact-session wake-source provider for roots and children and factor
  it with the existing root-tree budget projection using producer-ledger-first,
  inbox-last ordering.
- `internal/progressruntime/progress.go`, `internal/progress/render.go`, and
  `internal/sessionstore/progress_store.go` — split final rendering into a pure
  facts-to-text step and a transaction-scoped facts projection reusable by the
  response disposition; keep ordinary status/progress reads unchanged.
- `internal/sessionlifecycle/{completions,response_integrity}.go` and tests —
  derive the sixth-empty host notice from durable child session state and
  terminalize it as the successful parent-facing result.
- `internal/sessionstore/*completion*_test.go`,
  `response_integrity_*test.go`, `generation_protocol_model_test.go`, and
  `harness_model_test.go` — cover transaction atomicity, state-machine traces,
  budget precedence, input reset, output generation, and restart.
- `internal/session/*loop*_test.go`, `empty_response_output_test.go`, and
  `activation_terminal_test.go` — cover pure loop routing, pending activation
  expiry, candidate visibility, todo wording, reply carry, and empty escalation.
- `internal/daemon/*_scenario_test.go` and sanitized traces under
  `internal/testdata/harness_scenarios/` — prove root/subagent behavior,
  background bypass, external-input reset, crash recovery, and final rendered
  output through the production runner.
- `ARCHITECTURE.md`, `docs/glossary.md`, ADR-0053, and the new superseding ADR —
  describe finish evidence, wake-aware completion checking, durable reset and
  restart rules so the intentional second model call is not removed as a loop
  bug later.

## Tasks

### 1. Establish the durable completion protocol and migration

- Add the next free forward migration with nullable
  `sessions.completion_check_candidate_id` referencing `messages(id)` and
  `sessions.manager_reply_pending BOOLEAN NOT NULL DEFAULT FALSE`, plus
  `sessions.empty_stop_streak INTEGER NOT NULL DEFAULT 0` constrained
  non-negative. Extend `SessionRecord`, shared session-column scans, factory
  resume options, and migration coverage.
- Define typed response dispositions/results at the session-store boundary.
  Extend accepted-response persistence so the complete chosen transition commits
  assistant message, total iteration, budget observation, candidate set/clear,
  empty-streak change, optional host nudge, and optional output together.
- Include a terminal wake-projection-error disposition. It stores the assistant
  attempt and usage without making it active final output, observes budget first,
  and otherwise commits `status=error` plus one host error output for a root.
  Return a terminal-committed verdict so `session.run` does not write a second
  generic error state.
- Validate expected candidate identity when confirming or clearing. Treat a
  mismatched/stale identity as a persistence conflict rather than accepting an
  unrelated stop.
- Keep an unconfirmed candidate as an ordinary active assistant transcript row;
  insert its host nudge immediately after it in the same transaction. Do not use
  `rejected_reason` or `retry_of_message_id`.
- Make budget fire an explicit disposition returned to the daemon gate. Suppress
  both nudge and candidate text when an unconfirmed attempt crosses the budget.
- Refactor final progress capture so the response transaction can read
  post-disposition facts through its existing `*sql.Tx` and invoke a pure
  facts-to-final-text renderer supplied by the session/daemon boundary. Insert
  that exact text before commit. The renderer receives the transaction timestamp
  and must not call `CaptureProgress` or another store method.

Acceptance criteria:

- Existing databases migrate to `candidate=NULL`, `manager_reply_pending=FALSE`, and
  `empty_stop_streak=0`; a rerun of migration setup remains safe through the
  normal goose contract.
- No committed database state contains a candidate without its nudge, a nudge
  without its candidate identity, a confirmed/tool response with stale pending
  identity, or an incremented iteration without its response evidence.
- A wake-projection failure retains one attempt and its usage, emits no candidate
  text, and recovers after restart without re-running the model or duplicating
  the host error.
- Store protocol-model tests match production across candidate, restart,
  confirmation, tool reset, external-input reset, empty warning/terminal,
  budget fire, and another restart.
- A first unconfirmed candidate creates no `session_outbox` row. Confirmed and
  background-yield responses create the same output type/release behavior as
  the pre-change final path.
- The final footer committed with a confirmed/yield response includes that
  response's iteration, usage/cost and resulting budget state; a forced crash
  cannot leave the assistant row without its exact rendered outbox row.

Focused verification:

```bash
go test ./internal/migrate -run 'Test.*CompletionCheck'
go test ./internal/sessionstore -run 'Test.*(CompletionCheck|ResponseDisposition|ResponseIntegrity|Generation)'
```

### 2. Make every model-input ingress invalidate stale completion state

- Add one transaction helper that clears candidate identity and empty streak;
  call it from every transaction that commits external model-visible input.
  Trace all `advanceModelInputGeneration` callers plus direct external
  tool-result/subagent completion paths; do not assume inbox promotion is the
  only ingress.
- Set `manager_reply_pending` on manager-owned model-input promotion. Preserve it
  for every non-releasing output and non-manager ingress, and clear it only with
  a releasing response/error/budget output or terminal lifecycle settlement
  that supersedes the turn. Read-only command receipts do not clear it.
  Idempotent input or output replay must not clear a newer obligation.
- Clear on fresh/replayed user, agent, process, and subagent input promotion,
  scheduled and internal synthetic delivery, direct pending-call resolution,
  blocking child completion, and context reset. Idempotent delivery replays that
  insert no new model input must remain no-ops.
- Give completion-nudge insertion its own typed response-disposition path and
  deliberately omit the reset helper and model-input generation advancement
  there.
- Update the in-memory session projection only after the matching database
  commit, or reload it from `SessionRecord`/store after a recovered boundary.

Acceptance criteria:

- Each external ingress test starts from a pending candidate and non-zero empty
  streak, commits one model-visible input, and observes both fields cleared in
  the same database state as that input.
- A manager input followed by candidate → tool response → crash → ADR-0059
  settlement → new candidate → confirmation retains `manager_reply_pending`
  until the confirmed persistent output commits and then clears it.
- `/stop` and terminal shield/error settlement clear a pending manager reply in
  their existing atomic completion transactions; `/status`, `/help`, and other
  read-only receipts leave it unchanged.
- Duplicate scheduled/subagent/process delivery cannot clear a newer check when
  it inserts no new row.
- No production branch tests nudge text to decide whether reset is allowed.

Focused verification:

```bash
go test ./internal/sessionstore -run 'Test.*(Input|Delivery|Reset).*Completion'
go test ./internal/subagent -run 'Test.*Deliver.*Completion'
```

### 3. Implement the shared loop policy and conditional nudge

- Add one completion-policy decision point in `internal/session` after finish
  integrity normalization and before any assistant output. Use it for every
  agent type.
- Inject an exact-session durable wake-source callback from the daemon. Factor
  the existing budget projection so both callers share the definitions and
  ledger-first/inbox-last ordering while retaining exact-session versus
  root-subtree scope.
- Count non-blocking child links only in `spawned`, `running`, `completed`, or
  `error`. Add regression coverage showing that stopped/killed undelivered links
  neither bypass checking nor retain a budget on the promise of a wake that
  cannot occur.
- On wake present, retain current ordinary completion behavior and avoid every
  completion/empty nudge. On no wake and no pending check, commit a hidden
  candidate plus one nudge. On pending check, accept the next non-empty stop.
- Render open todo items in one helper. Include only `pending` and `in_progress`
  items; omit the entire todo clause when none exist. The prompt must permit a
  deliberate second stop with explanation and must not require `OK`.
- Preserve the original manager reply obligation across the internal turn and
  restart. Never publish or promote the first candidate through notification,
  progress, direct-reply, final-output, or max-iteration code.
- Replace the store-reading `FinalOutput(ctx, text)` response path with the pure
  post-disposition renderer used by the response transaction. Keep controller
  `/status` and ordinary progress refresh on their existing read-only path.
- Pass the pending candidate's transcript position into compaction and cap every
  legal split at or before it, retaining candidate and nudge as raw tail rows.
  Exercise both automatic and explicit compaction at the pressure boundary.
- Atomically clear completion state when a tool-bearing response is persisted;
  let normal execution and ADR-0059 recovery handle the calls afterward.

Acceptance criteria:

- The same scripted response sequence produces the same completion protocol for
  a root and a child session.
- Empty todo produces no todo wording; open todo lists the actionable entries;
  completed/cancelled entries are omitted. A second explained stop finishes even
  if actionable entries remain.
- A tool call after nudge clears the check; its next stop is hidden and checked
  again before the following stop becomes visible.
- A manager receives exactly one final response—the confirmed one—and its
  original accepted input is released only by that output.
- Final footer assertions include the current confirmed response's iteration,
  usage/cost and budget state, proving the renderer saw post-disposition facts.
- A subagent parent receives only the confirmed child result.

Focused verification:

```bash
go test ./internal/session -run 'Test.*(CompletionCheck|FinalOutput|Reply|Todo)'
go test ./internal/daemon -run 'Test.*CompletionCheck.*(Root|Child)'
```

### 4. Fold empty stops into durable loop detection

- Replace `loopRunner.emptyCount` with the loop detector's distinct empty-stop
  event backed by the durable session streak. Do not add a synthetic tool record
  to its diversity window and do not let tool warnings change empty thresholds.
- Run wake-source projection before empty handling. Without wake, persist each
  empty attempt and streak transition; retain ordinary nudge wording below three,
  the strong warning at three, and the terminal notice at six.
- Keep a pending completion candidate through empty attempts. A later non-empty
  stop may confirm it; a tool-bearing response or external input clears both
  states.
- Make the sixth-response host notice the ordinary successful final result for
  both roots and children. Key root output idempotently so restart cannot
  duplicate it; ensure child finalization can recover and deliver the same
  notice after a crash.
- Expose the same terminal notice to `sessionlifecycle` through the persisted
  streak/session record. A child with streak six must finalize with
  `OutcomeCompleted` and that notice, including when the daemon restarts before
  link terminalization; do not insert a synthetic assistant answer.

Acceptance criteria:

- Empty attempts 1–2 nudge, 3 warns, 4–5 nudge, and 6 finishes once with the
  durable notice. Restart at each boundary continues from the same next count.
- Six empty confirmations cannot loop forever or silently publish the hidden
  candidate.
- An external input resets the count; a host nudge does not. Empty stop with a
  wake source yields immediately at any prior count.
- Tool-diversity detector tests retain their existing verdicts and thresholds.

Focused verification:

```bash
go test ./internal/session -run 'Test.*(EmptyResponse|LoopDetector|CompletionCheck)'
go test ./internal/sessionstore -run 'Test.*EmptyStop'
```

### 5. Prove restart, wake races, output, and budget behavior end to end

- Add a sanitized scenario fixture preserving the reported order: user task →
  premature non-empty stop → hidden completion nudge → tool work → another
  hidden stop/nudge → confirmed final. Assert the manager never receives either
  candidate.
- Add root and subagent scenarios for a stop with advertised Bash/background
  child work. Assert one model call, no nudge, runner release, later durable
  completion wake, and the existing output shape. Include empty background
  yield.
- Restart after candidate+nudge commit and assert exactly one confirmation call
  and no duplicate nudge. Restart after confirmed output and assert no model
  call and exactly one releasing output. Restart after tool-call persistence and
  assert ADR-0059 settlement plus a fresh later completion check.
- Carry the crash-after-tool scenario through the new confirmation and output
  acknowledgement, asserting one persistent releasing manager answer and a
  cleared durable reply obligation.
- Race a process/child terminal transaction against wake projection. The trace
  must observe either live ledger or queued inbox and never create a no-wake
  nudge in the gap.
- Add stopped and killed undelivered child fixtures and prove neither is treated
  as a wake source.
- Cover an unexpected process/subagent input and a manager message arriving
  while confirmation is pending; promotion clears the old check, and the next
  stop begins a new one.
- Cover budget crossing on first candidate, between candidate and confirmation,
  and on confirmed stop. The first two paths never expose candidate text or
  spend an unadmitted confirmation call; the confirmed path keeps existing
  combined output semantics.
- Force wake-projection failure after the provider returns and assert durable
  usage, hidden candidate text, one terminal host error, budget precedence, and
  no model replay after restart.

Acceptance criteria:

- Production SQLite, daemon recovery, runner, inbox, outbox, and final renderer
  — not private flags — demonstrate every restart and visibility invariant.
- There is no exact nudge/acknowledgement marker in model output or test logic.
- The permanent scenario fixture fails if completion checking is later removed
  as an apparent extra-loop bug.

Focused verification:

```bash
go test ./internal/sessionstore -run 'TestHarnessModel.*Completion'
go test ./internal/daemon -run 'Test.*(CompletionCheck|StopDisposition|BackgroundYield).*'
```

### 6. Record the architecture contract and perform final handoff verification

- Add the superseding ADR and mark ADR-0053 superseded. Explain why ordinary
  background yield remains correct, why no-wake stops now require a durable
  second look, why exact markers and terminal tools remain rejected, and why a
  candidate message ID plus durable empty streak is preferred to process memory.
- Update `ARCHITECTURE.md` session activation, durable transcript, restart, and
  background sections to state the implemented state machine and its output
  boundary without turning the document into a test plan.
- Reconcile `docs/glossary.md` with the implemented names `model finish reason`,
  `background wake source`, and `completion check`; keep definitions aligned
  with code after naming settles.
- Follow the repository's final cadence: run the complete local suite once,
  invoke `pilat:arch-sync`, perform the required implementation handoff review,
  then delegate lint and CI reporting exactly as repository instructions require.

Acceptance criteria:

- `rg` finds no completion contract that depends on `OK`, `<WAITING/>`, a final
  tool, or an agent-type-specific prompt.
- Architecture and ADR text clearly distinguish provider stop evidence,
  activation yield with a wake source, hidden no-wake candidate, confirmed
  finish, and empty-loop termination.
- Focused tests are green, then `make test` and the final repository-prescribed
  handoff gates complete without hiding exit status.

Final verification sequence:

```bash
make test
# Run pilat:arch-sync and the repository-required review/lint/CI handoff steps.
```
