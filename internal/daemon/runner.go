package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessioncalls"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionlifecycle"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

type runner = sessionlifecycle.Runner[queuedSessionInput]

// maxEmptyLoopIterations caps consecutive no-input iterations that still ask to
// continue. A healthy drain re-loops at most once (then exits on no work); more
// than this means the loop is spinning without progress (e.g. a stale pending-work
// signal) — break instead of flooding notifications.
const maxEmptyLoopIterations = 3

// runSession is the main goroutine for a session.
// ctx is already cancelable through rs (set in ensureRunner).
func (s *svc) runSession(ctx context.Context, sessionID int64, rs runner) {
	errored := false
	publishIdle := false

	defer func() {
		s.finishRunner(ctx, sessionID, rs, &errored, publishIdle, recover())
	}()

	notify := func(n sessionevent.Notification) {
		s.routes.Publish(sessionID, n)
	}

	announced := false
	emptyRuns := 0

	for {
		cont, hadInput := s.runSessionIteration(
			ctx, sessionID, rs, notify, &announced, &publishIdle, &errored,
		)
		if !cont {
			return
		}

		if hadInput {
			emptyRuns = 0
			continue
		}

		emptyRuns++
		if emptyRuns >= maxEmptyLoopIterations {
			logger.Ctx(ctx).Named("daemon.runner").Error(
				"session_loop_spin_guard",
				zap.Int64("session_id", sessionID),
				zap.Int("empty_runs", emptyRuns),
			)

			return
		}
	}
}

//nolint:wsl_v5 // Teardown publishes readiness only after removing the live loop.
func (s *svc) finishRunner(
	ctx context.Context,
	sessionID int64,
	rs runner,
	errored *bool,
	publishIdle bool,
	panicValue any,
) {
	shuttingDown := s.shuttingDown.Load()

	if panicValue != nil {
		*errored = true

		logger.Ctx(ctx).Named("daemon.runner").Error(
			"session_panic", zap.Int64("session_id", sessionID), zap.Any("panic", panicValue))
	}

	if ctx.Err() != nil && !shuttingDown {
		*errored = true
	}

	cleanupCtx := context.WithoutCancel(ctx)
	if !shuttingDown {
		s.externalCalls.Abandon(cleanupCtx, sessionID)
	}

	var unlock func()
	var lockErr error
	fenced := !shuttingDown && ctx.Err() == nil
	if fenced {
		unlock, lockErr = s.lockSessionTree(ctx, sessionID)
		if lockErr != nil {
			logger.Ctx(cleanupCtx).Named("daemon.runner").Error(
				"finish_runner_fence_failed", zap.Int64("session_id", sessionID), zap.Error(lockErr),
			)
		}
	}
	normalFence := fenced && lockErr == nil

	leftover, deliver, continued := s.finishRunnerLocked(
		cleanupCtx, ctx.Err(), sessionID, rs, shuttingDown, *errored, normalFence,
	)
	if unlock != nil {
		s.publishOwnerlessIdleAfterTeardown(
			cleanupCtx, sessionID, publishIdle, shuttingDown, ctx.Err() != nil, continued,
		)
		unlock()
	}

	if !shuttingDown && !normalFence {
		leftover, deliver, continued = s.finishCancelledRunner(
			cleanupCtx, sessionID, leftover, *errored,
		)
	}

	if deliver != nil {
		deliver()
	}

	if !shuttingDown && ctx.Err() == nil {
		s.supervisor.DrainReady(cleanupCtx)
		if !continued && !*errored {
			s.restartPendingAfterExit(cleanupCtx, sessionID)
		}
		if *errored {
			completeUnprocessedInputs(leftover, fmt.Errorf("session %d failed before input delivery", sessionID))
		}
	} else if !shuttingDown {
		// /stop or /kill may win after a typed delivery was appended to the
		// runner. Complete awaited senders explicitly; durable ledgers retain the
		// underlying event for the appropriate resume/recovery policy.
		completeUnprocessedInputs(leftover, fmt.Errorf("session %d stopped before input delivery", sessionID))
	}
}

func (s *svc) finishCancelledRunner(
	ctx context.Context,
	sessionID int64,
	leftover []queuedSessionInput,
	errored bool,
) ([]queuedSessionInput, func(), bool) {
	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.runner").Error(
			"finalize_cancelled_runner_failed", zap.Int64("session_id", sessionID), zap.Error(err),
		)

		return leftover, nil, false
	}
	defer unlock()

	return leftover, s.finalizeChildLocked(ctx, sessionID, false, errored), false
}

func (s *svc) finishRunnerLocked(
	ctx context.Context,
	runErr error,
	sessionID int64,
	rs runner,
	shuttingDown, errored, fenced bool,
) ([]queuedSessionInput, func(), bool) {
	if fenced {
		if err := s.externalCalls.SettleResults(ctx, sessionID); err != nil {
			logger.Ctx(ctx).Named("daemon.apply").Error(
				"abandon_delivery_failed", zap.Int64("session_id", sessionID), zap.Error(err))
		}
	}

	info := rs.Info()

	if info.PreserveStopped && !shuttingDown {
		record, err := s.sessionStore.GetSession(ctx, sessionID)
		if err != nil {
			logger.Ctx(ctx).Named("daemon.runner").Error(
				"preserve_stopped_status", zap.Int64("session_id", sessionID), zap.Error(err),
			)
		} else if record.Status == sessionstore.SessionStatusActive {
			if err := s.sessionStore.UpdateSessionStatus(
				ctx, sessionID, sessionstore.SessionStatusStopped,
			); err != nil {
				logger.Ctx(ctx).Named("daemon.runner").Error(
					"preserve_stopped_status", zap.Int64("session_id", sessionID), zap.Error(err),
				)
			}
		}
	}

	leftover := s.supervisor.Finish(sessionID)

	if fenced && !shuttingDown && runErr == nil && !errored && s.rerouteRunnerInputsLocked(ctx, sessionID, leftover) {
		s.reconcileLatestReadiness(ctx, sessionID)

		return nil, nil, true
	}

	var deliver func()

	if fenced {
		deliver = s.finalizeChildLocked(ctx, sessionID, shuttingDown, errored)
	}

	// The runner is already deregistered above, so the teardown reconcile
	// observes no live loop and may publish idle for the latest releasing
	// output. An ack arriving later re-runs readiness through the controller.
	if !shuttingDown {
		s.reconcileLatestReadiness(ctx, sessionID)
	}

	return leftover, deliver, false
}

func (s *svc) rerouteRunnerInputsLocked(
	ctx context.Context,
	sessionID int64,
	inputs []queuedSessionInput,
) bool {
	routed := false

	for _, input := range inputs {
		if err := s.routeQueuedSessionInputLocked(ctx, sessionID, input); err != nil {
			input.complete(false, err)

			continue
		}

		routed = true
	}

	return routed
}

func (s *svc) restartPendingAfterExit(ctx context.Context, sessionID int64) {
	link, err := s.links.GetLink(ctx, sessionID)
	if err != nil {
		return
	}

	runnable, err := s.pendingInputRunnable(ctx, sessionID)
	if err != nil || !runnable || (link != nil && link.Terminal()) {
		return
	}

	if link == nil {
		if _, err := s.resumeRecoverableRoot(ctx, sessionID); err != nil {
			logger.Ctx(ctx).Named("daemon.runner").Error(
				"restart_pending_session", zap.Int64("session_id", sessionID), zap.Error(err))
		}

		return
	}

	if err := s.ensureSessionRunner(ctx, sessionID); err != nil {
		logger.Ctx(ctx).Named("daemon.runner").Error(
			"restart_pending_session", zap.Int64("session_id", sessionID), zap.Error(err))
	}
}

// runSessionIteration executes one iteration of the session loop. The first
// result reports whether the loop should continue; the second whether this
// iteration consumed real input (messages or notifications) — the spin guard in
// runSession uses it to bound consecutive no-input continuations.
func (s *svc) runSessionIteration( //nolint:funlen,gocyclo // Linear lifecycle with explicit cleanup at each boundary.
	ctx context.Context,
	sessionID int64,
	rs runner,
	notify func(sessionevent.Notification),
	announced *bool,
	publishIdle *bool,
	errored *bool,
) (bool, bool) {
	rec, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		logger.Ctx(ctx).Warn("session_record_not_found", zap.Int64("session_id", sessionID), zap.Error(err))
		return false, false
	}

	if !*announced {
		*announced = true

		s.announceSession(ctx, sessionID, rs, rec, notify)
	}

	info := rs.Info()
	idleEligible := ownerlessSession(rec)

	if err := s.externalCalls.SettleResults(ctx, sessionID); err != nil {
		*errored = true
		*publishIdle = idleEligible

		s.reportSessionUnstarted(ctx, sessionID, notify, err)

		return false, false
	}

	sess, runErr := s.createOrResumeSession(ctx, sessionID, info.WorkDir, rec, info.PreserveStopped)
	if runErr != nil {
		*errored = true
		*publishIdle = idleEligible

		logger.Ctx(ctx).Warn("session_create_failed", zap.Int64("session_id", sessionID), zap.Error(runErr))
		s.reportSessionUnstarted(ctx, sessionID, notify, runErr)

		return false, false
	}

	defer sess.Close()

	inputs, runErr := s.prepareSessionInputs(ctx, sessionID, rs, sess)
	if runErr != nil {
		*errored = true
		*publishIdle = idleEligible

		sess.Close()
		logger.Ctx(ctx).Named("daemon.runner").
			Error("session_inputs_failed", zap.Int64("session_id", sessionID), zap.Error(runErr))
		s.reportSessionUnstarted(ctx, sessionID, notify, runErr)

		return false, false
	}

	hasDurableInput, pendingErr := s.hasPendingDurableInput(ctx, sessionID)
	if pendingErr != nil {
		*errored = true
		*publishIdle = idleEligible

		sess.Close()
		s.reportSessionUnstarted(ctx, sessionID, notify, pendingErr)

		return false, false
	}

	recoveringAcceptedTurn := false
	if !hasDurableInput && len(inputs) == 0 && !sess.HasPendingWork() && !rs.HasRun() {
		recoveringAcceptedTurn, runErr = s.recoverableInputRunnable(ctx, sessionID)
		if runErr != nil {
			*errored = true
			*publishIdle = idleEligible

			sess.Close()
			s.reportSessionUnstarted(ctx, sessionID, notify, runErr)

			return false, false
		}
	}

	hadInput := hasDurableInput || len(inputs) > 0 || recoveringAcceptedTurn

	if !hadInput && !sess.HasPendingWork() {
		*publishIdle = idleEligible

		sess.Close()

		return false, false
	}

	if err := s.activateStoppedRootForScheduledTurn(ctx, rec, inputs); err != nil {
		*errored = true
		*publishIdle = idleEligible

		sess.Close()
		s.reportSessionUnstarted(ctx, sessionID, notify, err)

		return false, false
	}

	rs.SetService(sess)

	if rec.ParentID == 0 {
		s.wakeProgress()
	}

	s.registerScheduleTools(ctx, rec, sess)
	s.registerSubagentTools(ctx, sessionID, sess)
	s.registerMCPTools(ctx, rec, sess)
	s.registerConfigEditTool(ctx, rec, sess)
	s.registerBudgetTool(ctx, rec, sess)

	rs.MarkRun()

	// The live "main model working" flag follows the loop's engagement: the
	// session clears it before publishing a final response, so progress cards
	// rendered after the message reads background work, not working.
	runResult, runErr := s.executeSession(ctx, sess, notify, func(active bool) {
		if active {
			rs.SetService(sess)
		} else {
			rs.SetService(nil)
		}
	})

	if rec.ParentID == 0 {
		runErr = errors.Join(runErr, s.settleRootBudget(ctx, sessionID, runResult.Suspended, runErr, notify))
	}

	s.reconcileLatestReadiness(ctx, sessionID)
	s.deferNotices.record(sessionID, runResult.CompactionDeferAnnounced)

	rs.SetService(nil)

	if rec.ParentID == 0 {
		s.wakeProgress()
	}

	sess.Close()

	// The session's suspended state is on disk by now, so a config change it
	// staged is safe to write and restart into.
	s.externalCalls.Apply(ctx, sessionID, s.shuttingDown.Load)

	if runErr != nil {
		*errored = true
		*publishIdle = idleEligible

		s.handleRunError(ctx, sessionID, runResult.ErrorNotice, runErr, notify)

		return false, hadInput
	}

	if runResult.Suspended {
		s.publishWaiting(ctx, sessionID, notify)
		return false, hadInput
	}

	if info.PreserveStopped {
		reloaded, reloadErr := s.sessionStore.GetSession(ctx, sessionID)
		if reloadErr != nil {
			*errored = true

			s.reportSessionUnstarted(ctx, sessionID, notify, reloadErr)

			return false, hadInput
		}

		if reloaded.Status == sessionstore.SessionStatusStopped {
			return false, hadInput
		}
	}

	// Continue to drain any messages that arrived during this run.
	return true, hadInput
}

// activateStoppedRootForScheduledTurn reopens a root only after SQLite accepted
// a new standalone occurrence; duplicate delivery leaves the root parked.
func (s *svc) activateStoppedRootForScheduledTurn(
	ctx context.Context,
	rec *sessionstore.SessionRecord,
	inputs []sessionInput,
) error {
	if rec.ParentID != 0 || rec.Status != sessionstore.SessionStatusStopped || !hasScheduledTurn(inputs) {
		return nil
	}

	if err := s.sessionStore.UpdateSessionStatus(ctx, rec.ID, sessionstore.SessionStatusActive); err != nil {
		return fmt.Errorf("activate stopped root %d for scheduled turn: %w", rec.ID, err)
	}

	return nil
}

func hasScheduledTurn(inputs []sessionInput) bool {
	return slices.ContainsFunc(inputs, inputIsScheduledTurn)
}

func (s *svc) publishWaiting(
	ctx context.Context,
	sessionID int64,
	notify func(sessionevent.Notification),
) {
	projections := s.collectWaitingProjections(ctx, sessionID)
	if len(projections) == 0 {
		return
	}

	waits := make([]sessionevent.WaitItem, len(projections))
	for i, projection := range projections {
		waits[i] = projection.wait
	}

	if err := s.recordWaitingProgress(ctx, sessionID, projections); err != nil {
		logger.Ctx(ctx).Named("daemon.waiting").Warn("record_waiting_progress", zap.Error(err))
	}

	notify(sessionevent.Notification{
		Type: sessionevent.NotifyWaiting, Message: sessionevent.FormatWaiting(waits), Waiting: waits,
	})
}

func (s *svc) collectWaitingProjections(ctx context.Context, sessionID int64) []waitingProjection {
	projections := make([]waitingProjection, 0)

	if s.scheduleSvc != nil {
		sleeps, err := s.scheduleSvc.PendingSleeps(ctx, sessionID)
		if err != nil {
			logger.Ctx(ctx).Named("daemon.waiting").Warn("list_pending_sleeps", zap.Error(err))
		} else {
			for _, sleep := range sleeps {
				wakeAt := sleep.WakeAt
				projections = append(projections, waitingProjection{
					wait:     sessionevent.WaitItem{Kind: sessionevent.WaitSleep, WakeAt: &wakeAt},
					display:  map[string]any{"wake_at": wakeAt.Format(time.RFC3339)},
					identity: map[string]any{"tool_call_id": sleep.CallID},
				})
			}
		}
	}

	links, err := s.links.ListPendingChildLinks(ctx, sessionID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.waiting").Warn("list_subagents", zap.Error(err))
	} else {
		for _, link := range links {
			if link.Blocking && !link.Terminal() && link.State != subagent.StateStopped {
				projections = append(projections, waitingProjection{
					wait:     sessionevent.WaitItem{Kind: sessionevent.WaitSubagent, ChildID: link.ChildID},
					display:  map[string]any{"child_id": link.ChildID},
					identity: map[string]any{"child_id": link.ChildID, "activation_seq": link.ActivationSeq},
				})
			}
		}
	}

	sort.Slice(projections, func(i, j int) bool {
		return waitingIdentityKey(projections[i].identity) < waitingIdentityKey(projections[j].identity)
	})

	return projections
}

// recordWaitingProgress enqueues the durable waiting card for the projected
// set; the canonical replaceable row is its own dedupe, so nothing is returned.
func (s *svc) recordWaitingProgress(
	ctx context.Context,
	sessionID int64,
	projections []waitingProjection,
) error {
	causalID, err := waitingProgressCausalID(projections)
	if err != nil {
		return err
	}

	// A stale waiting card is dropped without a recapture retry: the newer
	// transition that moved the generation owns the next card.
	if _, _, err := s.enqueueProgressChangeFor(ctx, sessionID, causalID, false); err != nil &&
		!errors.Is(err, sessionstore.ErrProgressSuperseded) && !errors.Is(err, sessionstore.ErrOutputOwner) {
		return fmt.Errorf("enqueue progress: %w", err)
	}

	return nil
}

func waitingProgressCausalID(projections []waitingProjection) (string, error) {
	identities := make([]map[string]any, len(projections))

	for i, projection := range projections {
		identities[i] = projection.identity
	}

	identity, err := canonicalWaitingIdentities(identities)
	if err != nil {
		return "", fmt.Errorf("encode waiting identities: %w", err)
	}

	digest := sha256.Sum256(identity)
	hash := hex.EncodeToString(digest[:])

	return "waiting:" + hash, nil
}

type waitingProjection struct {
	wait     sessionevent.WaitItem
	display  map[string]any
	identity map[string]any
}

func waitingIdentityKey(identity map[string]any) string {
	if childID, child := positiveWaitingInt(identity["child_id"]); child {
		activation, _ := positiveWaitingInt(identity["activation_seq"])

		return fmt.Sprintf("0:%020d:%020d", childID, activation)
	}

	if callID, ok := identity["tool_call_id"].(string); ok {
		return "1:" + callID
	}

	return "2:invalid"
}

func positiveWaitingInt(value any) (int64, bool) {
	switch number := value.(type) {
	case int64:
		return number, number > 0
	case int:
		return int64(number), number > 0
	default:
		return 0, false
	}
}

func canonicalWaitingIdentities(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode waiting identities: %w", err)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(encoded, &items); err != nil {
		return nil, fmt.Errorf("decode waiting identities: %w", err)
	}

	sort.Slice(items, func(i, j int) bool { return bytes.Compare(items[i], items[j]) < 0 })

	canonical, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("encode canonical waiting identities: %w", err)
	}

	return canonical, nil
}

func (s *svc) ensureSessionRunner(ctx context.Context, sessionID int64) error {
	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return err
	}
	defer unlock()

	return s.ensureSessionRunnerLocked(ctx, sessionID)
}

func (s *svc) ensureSessionRunnerLocked(ctx context.Context, sessionID int64) error {
	if _, ok := s.supervisor.Lookup(sessionID); ok {
		return nil
	}

	rec, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session %d: %w", sessionID, err)
	}

	if rec.KilledAt != nil {
		return fmt.Errorf("session %d is killed", sessionID)
	}

	if rec.Status == sessionstore.SessionStatusStopping || rec.Status == sessionstore.SessionStatusStopped {
		return fmt.Errorf("session %d is %s", sessionID, rec.Status)
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	if err != nil {
		return fmt.Errorf("resolve project %d: %w", rec.ProjectID, err)
	}

	err = s.ensureRunnerLocked(ctx, sessionID, workDir, rec.ProjectID, nil)
	if errors.Is(err, admission.ErrNoCapacity) && rec.ParentID == 0 {
		s.enqueuePendingRunner(sessionID, workDir, rec.ProjectID)
		return nil
	}

	return err
}

// Failed starts preserve input; only the first failure for that work is reported.
// Shutdown cancellation leaves recovery to the next boot without an error receipt.
func (s *svc) reportSessionUnstarted(
	ctx context.Context,
	sessionID int64,
	notify func(sessionevent.Notification),
	err error,
) {
	if ctx.Err() != nil {
		return
	}

	message := fmt.Sprintf(
		"⚠️ Session error: %s\n\nThe session is still alive — send a message to retry.",
		logger.Redact(err.Error()),
	)

	reported, outputErr := s.lifecycleStore.RecordSessionStartFailure(ctx, sessionID, message)
	if outputErr != nil {
		logger.Ctx(ctx).Named("daemon.runner").Warn("enqueue_unstarted_error_output", zap.Error(outputErr))
		return
	}

	if !reported {
		return
	}

	notify(sessionevent.Notification{
		Type:    sessionevent.NotifyMessage,
		Message: message,
	})
}

func (s *svc) publishOwnerlessIdle(ctx context.Context, sessionID int64) {
	if s.HasActiveLoop(sessionID) {
		return
	}

	record, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.runner").Warn(
			"idle_owner_lookup_failed", zap.Int64("session_id", sessionID), zap.Error(err),
		)

		return
	}

	if !ownerlessSession(record) {
		return
	}

	s.routes.Publish(sessionID, sessionevent.Notification{
		Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle,
	})
}

func (s *svc) publishOwnerlessIdleAfterTeardown(
	ctx context.Context,
	sessionID int64,
	publishIdle, shuttingDown, stopped, continued bool,
) {
	if !publishIdle || shuttingDown || stopped || continued {
		return
	}

	s.publishOwnerlessIdle(ctx, sessionID)
}

// prepareSessionInputs applies causal results before standalone events, regardless
// of cross-producer arrival order. A new event may interrupt sleep, but it never
// jumps ahead of an uninterruptible external call. Failed/deferred awaited inputs
// are acknowledged to their producer so durable schedulers can retry honestly.
func (s *svc) prepareSessionInputs(
	ctx context.Context,
	sessionID int64,
	rs runner,
	sess session.Service,
) ([]sessionInput, error) {
	deliveries := rs.DrainInputs()
	ordered := orderSessionInputs(deliveries)

	applied, resolvedExternal, err := s.applyResolvingInputs(ctx, sessionID, sess, ordered)
	if err != nil {
		return nil, err
	}

	standalone, interruptedByEvent, err := s.applyStandaloneInputs(ctx, sessionID, sess, ordered)
	if err != nil {
		return nil, err
	}

	applied = append(applied, standalone...)
	resolvedExternal = resolvedExternal || interruptedByEvent

	if resolvedExternal {
		err := s.injectOwedCompletions(ctx, sess, sessionID)
		if err != nil {
			return nil, err
		}
	}

	return applied, nil
}

func (s *svc) applyResolvingInputs(
	ctx context.Context,
	sessionID int64,
	sess session.Service,
	ordered []queuedSessionInput,
) ([]sessionInput, bool, error) {
	applied := make([]sessionInput, 0, len(ordered))

	for idx, delivery := range ordered {
		input := delivery.input()
		if !inputResolvesExistingCall(input) {
			continue
		}

		wasApplied, err := s.injectSessionInput(ctx, sessionID, sess, input)
		if err != nil {
			delivery.complete(false, err)
			completeUnprocessedInputs(ordered[idx+1:], err)

			return nil, false, err
		}

		delivery.complete(wasApplied, nil)

		if wasApplied {
			applied = append(applied, input)
		}
	}

	return applied, len(applied) > 0, nil
}

func (s *svc) applyStandaloneInputs(
	ctx context.Context,
	sessionID int64,
	sess session.Service,
	ordered []queuedSessionInput,
) ([]sessionInput, bool, error) {
	applied := make([]sessionInput, 0, len(ordered))
	interruptedSleep := false

	for _, delivery := range ordered {
		input := delivery.input()
		if inputResolvesExistingCall(input) {
			continue
		}

		if reason := inputSleepInterruption(input); reason != "" {
			interrupted, err := s.interruptPendingSleeps(ctx, sessionID, sess, reason)
			if err != nil {
				delivery.complete(false, err)
				completeUnprocessedInputs(ordered, err)

				return nil, interruptedSleep, err
			}

			interruptedSleep = interruptedSleep || interrupted
		}

		if pending := sess.PendingExternalCalls(); len(pending) > 0 {
			err := fmt.Errorf(
				"%w: %s (%s)",
				errSessionInputDeferred,
				pending[0].ID,
				pending[0].Name,
			)
			delivery.complete(false, err)

			continue
		}

		wasApplied, err := s.injectSessionInput(ctx, sessionID, sess, input)
		if err != nil {
			delivery.complete(false, err)
			completeUnprocessedInputs(ordered, err)

			return nil, interruptedSleep, err
		}

		delivery.complete(wasApplied, nil)

		if wasApplied {
			applied = append(applied, input)
		}
	}

	return applied, interruptedSleep, nil
}

func orderSessionInputs(deliveries []queuedSessionInput) []queuedSessionInput {
	ordered := make([]queuedSessionInput, 0, len(deliveries))
	for _, delivery := range deliveries {
		if inputResolvesExistingCall(delivery.input()) {
			ordered = append(ordered, delivery)
		}
	}

	for _, delivery := range deliveries {
		if !inputResolvesExistingCall(delivery.input()) {
			ordered = append(ordered, delivery)
		}
	}

	return ordered
}

func completeUnprocessedInputs(deliveries []queuedSessionInput, err error) {
	for _, delivery := range deliveries {
		delivery.complete(false, err)
	}
}

func (s *svc) injectSessionInput(
	ctx context.Context,
	sessionID int64,
	sess session.Service,
	input sessionInput,
) (bool, error) {
	switch value := input.(type) {
	case pendingCallResultInput:
		resolved, err := s.externalCalls.Resolve(ctx, sessionID, sess, value)
		if err != nil {
			return false, fmt.Errorf("resolve external call for session %d: %w", sessionID, err)
		}

		return resolved, nil
	case blockingSubagentCompletionInput:
		return true, s.injectBlockingCompletion(ctx, sess, value.ChildID, value.CallID, value.ActivationSeq)
	case scheduleTickInput:
		applied, err := sess.InjectToolNotificationOnce(
			ctx, value.DeliveryID, tool.IDSchedule, value.Content,
		)
		if err != nil {
			return false, fmt.Errorf("inject schedule tick: %w", err)
		}

		return applied, nil
	case freshScheduleInput:
		applied, err := sess.ResetContextAndInjectOnce(ctx, value.DeliveryID, value.Prompt)
		if err != nil {
			return false, fmt.Errorf("reset context for fresh schedule: %w", err)
		}

		return applied, nil
	case inboxReadyInput:
		return false, nil
	default:
		return false, fmt.Errorf("unsupported session input %T", input)
	}
}

// announceSession publishes a NotifySessionCreated event the first time a session loop runs.
func (s *svc) announceSession(
	ctx context.Context,
	sessionID int64,
	rs runner,
	rec *sessionstore.SessionRecord,
	notify func(sessionevent.Notification),
) {
	info := rs.Info()
	projectName, _ := s.store.GetProjectName(ctx, info.ProjectID)
	name := fmt.Sprintf("%s - %d", projectName, sessionID)
	notify(sessionevent.Notification{
		Type:       sessionevent.NotifySessionCreated,
		Name:       name,
		WorkDir:    info.WorkDir,
		Attributes: rec.Attributes,
	})
}

// interruptPendingSleeps resolves every exact sleep call before cancelling its
// timer. Durable result first is load-bearing: if cancellation fails or the
// daemon crashes, a later timer delivery is an idempotent no-op instead of a
// sleeping session whose only wake-up was deleted.
func (s *svc) interruptPendingSleeps(
	ctx context.Context,
	sessionID int64,
	sess session.Service,
	reason string,
) (bool, error) {
	pending := sess.PendingExternalCalls()
	interrupted := false

	for _, call := range pending {
		if call.Name != tool.IDSleep {
			continue
		}

		if _, err := sess.ResolvePendingCall(ctx, call, reason); err != nil {
			return interrupted, fmt.Errorf("inject sleep interrupt result for %s: %w", call.ID, err)
		}

		interrupted = true
	}

	if !interrupted {
		return false, nil
	}

	if s.scheduleSvc != nil {
		n, err := s.scheduleSvc.CancelPendingSleeps(ctx, sessionID)
		if err != nil {
			logger.Ctx(ctx).Warn("cancel_sleep_schedules", zap.Int64("session_id", sessionID), zap.Error(err))
		} else if n > 0 {
			logger.Ctx(ctx).
				Info("sleep_interrupted_by_user", zap.Int64("session_id", sessionID), zap.Int64("cancelled", n))
		}
	}

	return true, nil
}

func (s *svc) createOrResumeSession(
	ctx context.Context,
	sessionID int64,
	workDir string,
	rec *sessionstore.SessionRecord,
	preserveStopped bool,
) (session.Service, error) {
	return s.openSession(ctx, sessionID, workDir, rec, preserveStopped)
}

func (s *svc) sessionInputBoundary(
	sessionID int64,
	rec *sessionstore.SessionRecord,
) session.InputBoundary {
	return s.inputFactory.Boundary(
		sessionID,
		func(ctx context.Context) (string, error) {
			current, err := s.CurrentProgress(ctx, sessionID)
			if err != nil {
				return "", err
			}

			return current.Rendered, nil
		},
		func(ctx context.Context) (string, bool, error) {
			return s.enqueueProgressChange(ctx, sessionID)
		},
		func() {
			if rec.ParentID == 0 {
				s.wakeProgress()
			}
		},
		func(ctx context.Context, text string) (string, error) {
			return s.renderFinalOutput(ctx, sessionID, text)
		},
	)
}

// isManagementSurface reports the service-topic role. Attributes cross JSON,
// so the marker may arrive as a bool or a string; only an explicit false-like
// value opts out.
func isManagementSurface(attrs map[string]any) bool {
	v, ok := attrs[controllerapi.SessionAttributeManagementSurface]
	if !ok || v == nil {
		return false
	}

	switch v := v.(type) {
	case bool:
		return v
	case string:
		return v != "" && v != "false" && v != "0"
	default:
		return true
	}
}

//nolint:funlen // The construction boundary keeps one coherent session policy snapshot.
func (s *svc) openSession(
	ctx context.Context,
	sessionID int64,
	workDir string,
	rec *sessionstore.SessionRecord,
	preserveStopped bool,
) (session.Service, error) {
	externalCalls, err := s.externalCalls.Pending(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load pending external calls for session %d: %w", sessionID, err)
	}

	repoRoot, err := s.sessionRepoRoot(ctx, rec)
	if err != nil {
		return nil, err
	}

	opts := session.CreateOptions{
		ID:              sessionID,
		WorkDir:         workDir,
		Model:           rec.Model,
		ProjectID:       rec.ProjectID,
		AgentType:       rec.AgentType,
		RootID:          rec.RootID,
		ReasoningLevel:  rec.ReasoningLevel,
		Iteration:       rec.Iteration,
		TodoItems:       rec.TodoItems,
		LastActivityAt:  rec.UpdatedAt,
		ContextBaseline: rec.ContextBaseline(),
		ResumeCompletionState: &sessionstore.CompletionCheckState{
			CandidateID:         rec.CompletionCheckCandidateID,
			ManagerReplyPending: rec.ManagerReplyPending,
			EmptyStopStreak:     rec.EmptyStopStreak,
		},
		RepoRoot: repoRoot,

		StagedExternalCalls: externalCalls,

		CompactionDeferAnnounced: s.deferNotices.announced(sessionID),
		InputBoundary:            s.sessionInputBoundary(sessionID, rec),
		ProcessService:           s.processSvc,
		PreserveStoppedStatus:    preserveStopped,
	}
	if rec.ParentID != 0 {
		opts.OnIterationPersisted = func(ctx context.Context, iteration int) {
			s.publishSubagentIterationProgress(ctx, sessionID, int64(iteration))
		}
	}

	owner, _ := rec.Attributes[controllerapi.SessionAttributeManagerID].(string)

	opts.OutputEnabled = rec.ParentID == 0 && owner != ""
	budgetGateNeeded := owner != ""

	if rec.ParentID != 0 && s.budgetSvc != nil {
		budgetRecord, budgetErr := s.budgetSvc.Get(ctx, sessionRootID(rec))
		if budgetErr != nil && !errors.Is(budgetErr, sessionstore.ErrBudgetNotFound) {
			return nil, fmt.Errorf("load child budget gate: %w", budgetErr)
		}

		budgetGateNeeded = budgetErr == nil && budgetRecord.State != sessionstore.BudgetReleased
	}

	if s.budgetSvc != nil && budgetGateNeeded {
		opts.BudgetGate = &sessionBudgetGate{
			daemon: s, service: s.budgetSvc, store: s.runtimeStore,
			sessionID: rec.ID, rootID: sessionRootID(rec),
		}
	}

	opts.ActiveSubagents = s.activeSubagentInfos(ctx, sessionID)
	opts.ActiveSubagentsProvider = func(ctx context.Context) []session.ActiveSubagentInfo {
		return s.activeSubagentInfos(ctx, sessionID)
	}
	opts.ActiveProcesses = s.activeProcessInfos(ctx, sessionID)
	opts.ActiveProcessesProvider = func(ctx context.Context) []session.ActiveProcessInfo {
		return s.activeProcessInfos(ctx, sessionID)
	}

	// Subagents never carry the instruction: the attribute marks roots only,
	// and a resumed root reattaches it through this same open path.
	if rec.ParentID == 0 && isManagementSurface(rec.Attributes) {
		skill, err := loader.BuiltinSkill(loader.ManagementSkillName)
		if err != nil {
			return nil, fmt.Errorf("load management skill: %w", err)
		}

		opts.ExtraSkills = append(opts.ExtraSkills, skill)
	}

	sess, err := s.factory.Create(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	return sess, nil
}

// sessionRepoRoot follows the durable tree root because child session rows do
// not copy manager-owned attributes from their parent.
func (s *svc) sessionRepoRoot(ctx context.Context, rec *sessionstore.SessionRecord) (string, error) {
	if rec.RootID == 0 {
		repoRoot, _ := rec.Attributes["repo_root"].(string)

		return repoRoot, nil
	}

	root, err := s.sessionStore.GetSession(ctx, rec.RootID)
	if err != nil {
		return "", fmt.Errorf("load root session %d for worktree policy: %w", rec.RootID, err)
	}

	if root == nil || root.ProjectID != rec.ProjectID {
		return "", fmt.Errorf("root session %d does not match project %d", rec.RootID, rec.ProjectID)
	}

	repoRoot, _ := root.Attributes["repo_root"].(string)

	return repoRoot, nil
}

// activeSubagentInfos maps a session's pending (undelivered) child links to the
// summary the session pins in its active-background prompt section. Empty for
// a leaf session with no children.
func (s *svc) activeSubagentInfos(ctx context.Context, sessionID int64) []session.ActiveSubagentInfo {
	links, err := s.links.ListPendingChildLinks(ctx, sessionID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.runner").
			Warn("list_pending_child_links", zap.Int64("session_id", sessionID), zap.Error(err))

		return nil
	}

	if len(links) == 0 {
		return nil
	}

	infos := make([]session.ActiveSubagentInfo, 0, len(links))
	for _, l := range links {
		infos = append(infos, session.ActiveSubagentInfo{
			ChildID:  l.ChildID,
			Blocking: l.Blocking,
			State:    string(l.State),
		})
	}

	return infos
}

func (s *svc) activeProcessInfos(ctx context.Context, sessionID int64) []session.ActiveProcessInfo {
	if s.processStore == nil {
		return nil
	}

	processes, err := s.processStore.ListRunningBySessions(ctx, []int64{sessionID})
	if err != nil {
		logger.Ctx(ctx).Named("daemon.runner").
			Warn("list_running_processes", zap.Int64("session_id", sessionID), zap.Error(err))

		return nil
	}

	infos := make([]session.ActiveProcessInfo, 0, len(processes))
	for _, process := range processes {
		if process.AdvertisedAt == nil {
			continue
		}

		infos = append(infos, session.ActiveProcessInfo{ID: process.ID, OutputPath: process.OutputPath})
	}

	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })

	return infos
}

func (s *svc) executeSession(
	ctx context.Context,
	sess session.Service,
	notify func(sessionevent.Notification),
	working func(bool),
) (session.RunResult, error) {
	notify(sessionevent.Notification{Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateRunning})

	result, runErr := sess.RunDaemon(ctx, notify, working)

	if runErr != nil {
		return result, fmt.Errorf("run session: %w", runErr)
	}

	return result, nil
}

// handleRunError processes errors from RunDaemon and sends appropriate notifications.
// The caller is responsible for closing the session before calling this.
func (s *svc) handleRunError(
	ctx context.Context,
	sessionID int64,
	message string,
	runErr error,
	notify func(sessionevent.Notification),
) {
	if ctx.Err() != nil {
		// Shutdown — don't flush or notify (sessions will be resumed on restart).
		return
	}

	logger.Ctx(ctx).Warn("session_error", zap.Int64("session_id", sessionID), zap.Error(runErr))

	if message == "" {
		message = fmt.Sprintf(
			"⚠️ Session error: %s\n\nThe session is still alive — send a message to continue.",
			logger.Redact(runErr.Error()),
		)
	}

	notify(sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: message})
}

// registerScheduleTools registers daemon-mode schedule and sleep tools on the session's live registry.
func (s *svc) registerScheduleTools(
	ctx context.Context,
	rec *sessionstore.SessionRecord,
	sess session.Service,
) {
	if s.scheduleSvc == nil {
		return
	}

	if rec.ParentID == 0 {
		registerLogged(ctx, sess, schedule.NewScheduleTool(rec.ID, s.scheduleSvc, time.Local))
	}

	registerLogged(
		ctx,
		sess,
		s.guardSleepWhileBackgroundPending(rec.ID, schedule.NewSleepTool(s.scheduleSvc, rec.ID)),
	)
}

// registerMCPTools registers the MCP registry tools. Root sessions only: a
// subagent must not reshape the toolset its parent will run with.
func (s *svc) registerMCPTools(ctx context.Context, rec *sessionstore.SessionRecord, sess session.Service) {
	if s.mcpStore == nil || rec.ParentID != 0 {
		return
	}

	onChange := func(project *int64) {
		var projectID int64
		if project != nil {
			projectID = *project
		}

		if err := s.toolResources.Invalidate(projectID); err != nil {
			logger.Ctx(ctx).Warn("invalidate_mcp_resources", zap.Error(err))
		}
	}
	for _, t := range newMCPTools(s.mcpStore, rec.ProjectID, onChange) {
		registerLogged(ctx, sess, t)
	}
}

// registerConfigEditTool registers the /config-gated full-document editing tool
// on every root session with an applier. Subagents are excluded: they cannot
// acquire a manager-owned activation grant, and the tool must not be reachable
// from them at all.
func (s *svc) registerConfigEditTool(
	ctx context.Context,
	rec *sessionstore.SessionRecord,
	sess session.Service,
) {
	if rec.ParentID != 0 {
		return
	}

	if edit := s.externalCalls.ConfigEditTool(rec.ID); edit != nil {
		registerLogged(ctx, sess, edit)
	}
}

// registerSubagentTools registers the daemon-mode task and subagent-monitor
// tools on the session's live registry. The daemon itself is the spawner, so
// these are gated the same as schedule tools — availability still follows the
// session's agent-type allowlist.
func (s *svc) registerSubagentTools(ctx context.Context, sessionID int64, sess session.Service) {
	var skills loader.SkillCatalog
	if catalog, ok := sess.(interface{ SkillCatalog() loader.SkillCatalog }); ok {
		skills = catalog.SkillCatalog()
	}

	for _, t := range []tool.Tool{
		newTaskTool(s, sessionID, sess.AgentTypes(), s.modelCatalog, skills),
		newGetSubagentResultTool(s),
		newSendToSubagentTool(s),
	} {
		registerLogged(ctx, sess, t)
	}
}

// registerLogged registers t on sess, logging at Debug when the session's
// agent-type allowlist rejected it.
func registerLogged(ctx context.Context, sess session.Service, t tool.Tool) {
	if !sess.RegisterGatedTool(t) {
		logger.Ctx(ctx).Debug("tool_gated_out", zap.String("tool_id", t.ID()))
	}
}

func (s *svc) ensureRunner(
	ctx context.Context,
	sessionID int64,
	workDir string,
	projectID int64,
	inputs []queuedSessionInput,
) error {
	if s.shuttingDown.Load() {
		return errDaemonShuttingDown
	}

	if err := s.supervisor.Ensure(ctx, sessionID, workDir, projectID, inputs); err != nil {
		return fmt.Errorf("ensure session runner: %w", err)
	}

	return nil
}

func (s *svc) ensureRunnerLocked(
	ctx context.Context,
	sessionID int64,
	workDir string,
	projectID int64,
	inputs []queuedSessionInput,
) error {
	if s.shuttingDown.Load() {
		return errDaemonShuttingDown
	}

	if err := s.supervisor.EnsureLocked(ctx, sessionID, workDir, projectID, inputs); err != nil {
		return fmt.Errorf("ensure session runner: %w", err)
	}

	return nil
}

func (s *svc) ensureRunnerStartable(
	ctx context.Context,
	rec *sessionstore.SessionRecord,
	inputs []queuedSessionInput,
) (bool, error) {
	stored, err := s.sessionStore.LoadActiveMessages(ctx, rec.ID)
	if err != nil {
		return false, fmt.Errorf("load session %d transcript for runner admission: %w", rec.ID, err)
	}

	if _, err := sessioncalls.ScanStored(stored); err != nil {
		return false, fmt.Errorf("session %d requires transcript repair: %w", rec.ID, err)
	}

	preserveStopped, err := s.commandOnlyStoppedRoot(ctx, rec)
	if err != nil {
		return false, err
	}

	if err := validateRunnerStart(rec, inputs, preserveStopped); err != nil {
		return false, err
	}

	return preserveStopped, nil
}

func validateRunnerStart(rec *sessionstore.SessionRecord, inputs []queuedSessionInput, preserveStopped bool) error {
	if rec.KilledAt != nil || rec.Status == sessionstore.SessionStatusKilled {
		return fmt.Errorf("session %d is killed", rec.ID)
	}

	if rec.Status == sessionstore.SessionStatusStopping ||
		(rec.Status == sessionstore.SessionStatusStopped && !preserveStopped &&
			(rec.ParentID != 0 || !queuedInputsStartScheduledTurn(inputs))) {
		return fmt.Errorf("session %d is %s", rec.ID, rec.Status)
	}

	return nil
}

// commandOnlyStoppedRoot reports whether a stopped root is being woken for a
// read-only boundary command: those run while the root stays parked, unlike
// ordinary accepted work which reactivates it.
func (s *svc) commandOnlyStoppedRoot(ctx context.Context, rec *sessionstore.SessionRecord) (bool, error) {
	if rec.Status != sessionstore.SessionStatusStopped || rec.ParentID != 0 {
		return false, nil
	}

	pending, err := s.hasPendingDurableInput(ctx, rec.ID)
	if err != nil {
		return false, err
	}

	if !pending {
		return false, nil
	}

	head, err := s.inboxStore.PeekPending(ctx, rec.ID)
	if err != nil {
		return false, fmt.Errorf("peek stopped root input: %w", err)
	}

	return isReadOnlyBoundaryCommand(head.RawContent), nil
}

func queuedInputsStartScheduledTurn(inputs []queuedSessionInput) bool {
	return len(inputs) > 0 && !slices.ContainsFunc(inputs, func(input queuedSessionInput) bool {
		return !inputIsScheduledTurn(input.input())
	})
}
