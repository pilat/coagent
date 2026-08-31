# ADR-0015: One config apply in flight, and a marker consumed exactly once

- **Status:** Accepted
- **Date:** 2026-08-16

## Context

[ADR-0008](0008-config-tools-restart-apply.md) made the pending-apply marker the
only record that a session is suspended on a config call across the restart the
apply itself causes. Two holes in that design were reachable in ordinary use:

- The "one staged change at a time" guard was **per session**, while the marker,
  `config.yaml` and the restart are process-global. Two root sessions — or one
  session plus the bootstrap `set_provider` socket op — could commit
  concurrently. The second commit overwrote the first session's marker: that
  session's change was silently discarded, and its suspended call was left with
  no producer that owed it a result, which no later boot could repair.
- A marker whose verdict could not be delivered was kept, deliberately, so the
  next boot could retry. Nothing bounded that: for a killed or stopped session
  the retry can never succeed, so the marker stayed armed forever and the first
  *unrelated* boot failure — a delisted model id, an unreachable catalog — rolled
  a config that had been live for days back to its backup.

Both are ordering problems around a single-slot durable token, not bugs in any
one function.

## Decision

The apply pipeline has **one slot per process image**, and a marker is consumed
by **exactly one** boot resolution.

- `configapply.Service.ClaimApply`/`ReleaseApply` is the single gate. Session tools take
  it through `svc.stageApply`; the bootstrap `set_provider` op takes it directly.
  A caller that cannot take the slot is refused **before** any side effect — the
  tool errors instead of suspending, so no call is left unowned, and
  `set_provider` claims the slot ahead of the credential write so a refusal
  leaves no orphan secret on disk.
- The slot returns only when the commit failed and no restart is coming
  (`Apply` releases it, and the rejection is delivered in-process). A committed
  change keeps the slot for the rest of the process image; the restart clears it.
- **Slot release has exactly two shapes, and every path must end in one.** The
  slot returns either because the commit failed (`ReleaseApply`, in-process
  rejection) or because a restart replaces the process image. A claim that reaches
  neither is unrecoverable: the slot is in-memory, so nothing later in the same
  image can free it, and every subsequent config change — from any session and
  from the bootstrap socket — is refused until the daemon happens to restart. Two
  paths did not end in one of the two shapes and now do:
  - A **committed** bootstrap change scheduled its restart with `Conn.AfterReply`,
    which `ctl` skips when the response cannot be written. `restartOnCommit` arms
    the restart on `Conn.Done` as well; the reply decides the order, never whether
    the daemon comes back.
  - A **session loop that dies between the claim and the commit** (a panic, which
    the runner recovers so the daemon survives) wrote nothing and asked for no
    restart. `finishRunner` now calls `abandonStagedApply`: it takes the staged
    change back, releases the slot and answers the call the loop owed. It is a
    no-op on healthy paths, because the change is handed over exactly once.

- **A marker is armed only for a durably suspended call.** The marker names one
  exact `{session_id, tool_call_id}`, and only the durable transcript can back
  that name. `runStagedApply` re-reads the transcript before it commits (the same
  name-keyed scan the boot sweep uses) and commits only when the call is there and
  unresolved under its own tool name. Committing on the strength of the in-memory
  ledger alone would arm a marker every boot fails to deliver — and since the
  session is alive, that failure classifies as transient, so the marker survives
  forever and re-creates the "an armed marker turns a later unrelated `bootErr`
  into a silent rollback" failure this ADR closed. A staged change that fails the
  check is dropped in-process: the slot is released, nothing is written, and the
  ledger entry goes with it, because there is no call waiting for an answer. Only
  when the transcript *read* itself fails is the call also answered, since a
  pending call with no producer bricks the session it belongs to.

- `deliverApplyVerdict` classifies a failed delivery instead of always retrying.
  A session that is missing, killed, stopping or stopped can never take the
  verdict: it is logged as undeliverable and the marker is cleared. Any other
  failure is transient and keeps the marker for the next boot.
- `ClearPending(p)` removes only the marker instance it resolved, comparing the
  bytes on disk with `p`. A marker written by a newer apply belongs to that
  apply's own waiting call.

## Consequences

- A second config change during an apply is an ordinary tool error the model can
  read and retry after the restart, instead of a silent loss.
- The bootstrap socket op can now be refused mid-onboarding while a chat session
  is applying. That is the correct answer, and the caller is told why.
- A verdict can be lost when its session is killed — but loudly, in one log line,
  and only for a session that could not have received it anyway. That is
  strictly better than arming every later boot.
- A stranded slot is a same-process hazard only: the slot and the staged-call
  ledger both die with the image, and a claim that never committed leaves no
  marker, so the next boot's sweep closes the config call it left dangling. That
  is the recovery, not the design — within one image there is nothing to sweep.
- The apply pipeline pays one transcript read per commit. That is the price of not
  trusting an in-memory ledger about a durable fact; the read is the same one the
  boot sweep already does, and it happens once per config change.
- The credential the bootstrap `set_provider` writes before staging is *not* rolled
  back when the stage or the commit then fails. The write order is fixed the other
  way (a config referencing an undefined `${VAR}` is fatal at the next boot, an
  unreferenced secret is inert), `SetSecret` has no undo, and both plausible
  rollbacks are worse than the orphan: deleting the line breaks an existing
  provider that references the same variable, and restoring the file clobbers a
  concurrent secret write. The retry overwrites it in place.
- `ClearPending` takes the `Pending` it is acknowledging; the marker's bytes are
  its identity, so any future field added to `Pending` participates in that
  identity automatically.
- The write order (backup → marker → config) is now made durable: both writes
  fsync the containing directory after the rename, so the ordering survives power
  loss rather than only a process death.

## Alternatives Considered

- **Serialize inside `configops.Commit`.** Prevents the overwrite but far too
  late: the losing session has already suspended, so it still ends up with a call
  no producer owns. The refusal has to happen at stage time.
- **A retry counter in the marker.** Bounds the arming without needing session
  state, but picks an arbitrary number of boots and still discards the verdict of
  a session that was alive the whole time and merely busy.
- **Queue the second apply behind the first.** The queued change was staged
  against the pre-apply config, so it would have to be re-validated and re-staged
  after the restart — which is exactly what asking the model to retry does, with
  no durable queue to get wrong.
- **A daemon-wide lock held across the restart.** There is nothing to hold it in:
  the process is replaced. The marker already plays that role, and the in-memory
  slot only has to cover the window before the exec.
