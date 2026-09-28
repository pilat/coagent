# Durable settlement and joined heartbeat lifetime

## Goal

Close the two remaining protocol/lifetime gaps found by the
[core ownership audit](docs/refactoring-audit.md). Existing private owners are
cohesive; these corrections complete their contracts rather than add components.

## T1: Tool-result durability independent of presentation

The daemon's external-call coordinator opens transcript settlement with a real
runtime store and no output store. messageStore currently interprets missing
outputs as permission to call raw InsertMessage (and its batch path uses raw
InsertMessages). This loses the canonical result transaction's idempotency and
atomic completion-check invalidation during recovery.

Make the canonical tool-result commit a required RuntimeStore capability:
embed DirectOutputStore there and remove its redundant RuntimeOutputStore
embedding once consumers no longer use it for persistence. Retain the existing
transaction and schema, but persist tool-result attachments and include them in
replay identity: the current canonical insert omits both. Its direct output is
optional; its result identity, attachments and invalidation are not.

For every messageStore with durable storage, commit single and batched tool
results through store.InsertToolResultSetOnce. When no output capability was
provided, pass no DirectMessages, preserving presentation suppression. Only a
messageStore with no durable store may use the explicit in-memory fixture path.
Do not discover the result capability by assertion or select a weaker durable
algorithm on a missing dependency. OpenTranscript remains model/tool-free and
receives this capability through its explicit RuntimeStore dependency.

Preserve transcript order, returned row identities, tool-error/image fields,
normal response serialization, recovery/stop fences and error handling. Update
test doubles at the real result-commit seam; mocks used for isolated failures
must not simulate a second executable protocol. Behavioral scenarios continue
using migrated SQLite. A test that previously failed InsertMessage during
settlement must now fail the canonical batch operation or its SQL insert.

Affected: sessionstore/store.go and output_types.go; session/message_output.go
and affected fixtures; daemon/external_calls_test.go failure decorator; explicit
store-interface implementers identified by compiler/diagnostics. No factory
discovery, new ledger or changes to public controller contracts.

Acceptance: no durable tool-result path falls back to raw insert because
presentation is absent. Add a real-SQLite OpenTranscript regression with nil
output store proving fresh settlement invalidates the pending completion check,
replay creates no duplicate and does not invalidate a newer check, and a failed
result transaction preserves pending state. Verify both single and batch
messageStore entry paths without outputs, including attachment reload and
conflicting attachment replay. Exercise replay through a stale projection or
direct messageStore entry, because an already-resolved transcript skips the
store. Replace raw-insert attachment fixtures with the real entry paths and use
distinct call identities for distinct image reads. Re-run exact interrupted/orphan/stop
recovery scenarios and failed-settlement retention scenarios.

## T2: Heartbeat stop cancels and joins its worker

heartbeatTicker already owns its callback, mutex and cancel handle. Add a
completion signal for its worker. stop must cancel and wait for that generation
to exit before returning; runLoop's existing deferred stop is the join boundary.
Do not hold a lock needed by completion while waiting, and do not clear the
generation early enough to allow an overlapping restart. Preserve nil callback,
duplicate start, repeated stop and callback-panic recovery behavior. The
production callback publishes a nonblocking event; it never stops its own ticker.

Affected: internal/session/heartbeat.go and focused lifecycle tests. No new
worker manager or timeout that falsely claims a still-running callback joined.
Use deterministic callback-entry/cancellation/release signals to prove stop
waits until callback exit, and restart occurs only after the old generation
finishes. Keep all test waits bounded; do not depend on arbitrary sleeps.

## Verification and constraints

Read docs/testing.md. Use focused tests at these two checkpoints, then share
the final integrated specification review, delegated lint/canonical CI, make all
and independent code review with the existing plans. Keep exact evidence.
Preserve ongoing package ownership when editing; no full suite midway, no
mutation, shortened CI budgets, real home/credentials, staging or commits.

## Out of scope, accepted

No new architecture extraction, SQL-receiver split, general goroutine framework
or product behavior change. The corrections restore already-required durable
result and lifetime guarantees. Host output, candidate confirmation and budget
policy retain their canonical owners.

## Execution record

- Architecture audit found these two concrete gaps after confirming the model,
  tool-turn, checkpoint, external-call and manager-route owners.
- Cold plan validation found that canonical insertion omitted attachments;
  the scope now explicitly preserves them and checks replay identity. All other
  capability and lifetime decisions passed review; follow-up validation closed
  the attachment finding.
- Store capability migration and attachment persistence/identity are complete.
  New real-SQLite regressions failed before correction (10.655s), then 15 focused
  attachment, rollback, replay, invalidation, output and fence tests passed
  (18.091s).
- Session canonical settlement passed nil-output recovery, stale replay, failure
  rollback, single/batch attachment reload/conflict and existing image/error
  cases (5.803s). Deterministic virtual-clock heartbeat tests passed cancellation,
  blocked callback, joined stop, restart and panic recovery (0.033s).
- Architecture sync documented both contracts. Its constructor-style finding
  was corrected: checkpoint capabilities are explicit positional arguments;
  only activation data is grouped as options. A generated fixture cleanup
  accidentally removed test blocks. The missing committed scenarios were
  reconstructed from the baseline while preserving surviving local changes;
  49 restored session tests passed (66.407s), and the 1,000-iteration hard
  ceiling passed separately (160.433s). A pre-cleanup snapshot of the local
  assertions was unavailable; independent review checked the recovered coverage
  against all committed test and literal subtest names.
- Focused daemon stop/orphan/compaction/registry scenarios passed (12.195s),
  Telegram trace rendering passed (18.062s), and composition-root lifecycle
  tests passed (2.077s). The model-effort daemon case and image pixel case use
  `httptest.NewServer`, which cannot bind a TCP listener in this sandbox;
  their earlier phase checks passed before that environment restriction.
- The full local `make test` ran once and exited 2. Every failure shown in its
  log was a TCP or Unix listener denied by the sandbox (`setsockopt: operation
  not permitted`); other packages completed. Keep this as an environment
  limitation, not a green gate or a reason to weaken socket scenarios.
- Cold specification and independent code reviews are clean after the
  committed-budget handoff fix. The canonical CI run passed format, build,
  lint, architecture, Semgrep and gitleaks; its integration stage failed on
  sandbox listener and privileged-process restrictions. Final `make all` passed
  its static stages and failed the same bounded listener tests. The full goal
  remains active until those suites pass in a capable environment.
- In an unrestricted environment, full local tests passed. Canonical CI then
  found one stale prompt dependency in a migrated exact-cutoff test fixture;
  its focused correction, the full CI rerun and final `make all` passed.
