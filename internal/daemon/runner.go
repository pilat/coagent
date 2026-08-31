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
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionlifecycle"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

type runner = sessionlifecycle.Runner[queuedSessionInput]

// defaultBlockingTimeoutSec bounds a blocking child's wall-clock when the task
// tool did not specify one (preserves the legacy 5-minute task timeout).
const defaultBlockingTimeoutSec = 300

// maxEmptyLoopIterations caps consecutive no-input iterations that still ask to
// continue. A healthy drain re-loops at most once (then exits on no work); more
// than this means the loop is spinning without progress (e.g. a stale pending-work
// signal) — break instead of flooding notifications.
const maxEmptyLoopIterations = 3

// errNoChildTimeout is applyChildTimeout's non-error "no timeout applies" signal.
var errNoChildTimeout = errors.New("no timeout applies")

// runSession is the main goroutine for a session.
// ctx is already cancelable through rs (set in ensureRunner).
func (s *svc) runSession(ctx context.Context, sessionID int64, rs runner) {
	errored := false

	// Registered before the timeout is applied so every exit below runs the full
	// teardown; cancelling last keeps ctx.Err() meaningful inside it.
	var timeoutCancel context.CancelFunc

	defer func() {
		s.finishRunner(ctx, sessionID, rs, &errored, timeoutCancel, recover())
	}()

	// Blocking children get a wall-clock timeout; background children and parents
	// do not (bounded by MaxIterations / parent-kill orphaning) — Appendix G6.
	newCtx, cancel, ok := s.startChildTimeout(ctx, rs, sessionID, &errored)
	if !ok {
		return
	}

	ctx = newCtx
	timeoutCancel = cancel

	notify := func(n sessionevent.Notification) {
		s.publish(sessionID, n)
	}

	announced := false
	emptyRuns := 0

	for {
		cont, hadInput := s.runSessionIteration(ctx, sessionID, rs, notify, &announced)
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
	timeoutCancel context.CancelFunc,
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

	s.runners.Delete(sessionID)
	info := rs.Info()
	s.admit.Release(info.Kind, info.ParentID)

	cleanupCtx := context.WithoutCancel(ctx)

	if !shuttingDown {
		s.abandonStagedApply(cleanupCtx, sessionID)
	}

	s.finalizeChild(cleanupCtx, sessionID, shuttingDown, *errored)
	if !shuttingDown {
		s.reconcileLatestReadiness(cleanupCtx, sessionID)
	}

	leftover := rs.DrainInputs()
	rs.Complete()

	if !shuttingDown && ctx.Err() == nil {
		for _, input := range leftover {
			_ = s.routeQueuedSessionInput(cleanupCtx, sessionID, input)
		}

		s.drainPendingRunners(cleanupCtx)
		s.drainQueue(cleanupCtx)
		s.restartPendingAfterExit(cleanupCtx, sessionID)
	} else if !shuttingDown {
		// /stop or /kill may win after a typed delivery was appended to the
		// runner. Complete awaited senders explicitly; durable ledgers retain the
		// underlying event for the appropriate resume/recovery policy.
		for _, input := range leftover {
			input.complete(false, fmt.Errorf("session %d stopped before input delivery", sessionID))
		}
	}

	if timeoutCancel != nil {
		timeoutCancel()
	}
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

	sess, runErr := s.createOrResumeSession(ctx, sessionID, info.WorkDir, rec, info.PreserveStopped)
	if runErr != nil {
		logger.Ctx(ctx).Warn("session_create_failed", zap.Int64("session_id", sessionID), zap.Error(runErr))
		s.reportSessionUnstarted(ctx, sessionID, notify, runErr)

		return false, false
	}

	inputs, runErr := s.prepareSessionInputs(ctx, sessionID, rs, sess)
	if runErr != nil {
		sess.Close()
		logger.Ctx(ctx).Named("daemon.runner").
			Error("session_inputs_failed", zap.Int64("session_id", sessionID), zap.Error(runErr))
		s.reportSessionUnstarted(ctx, sessionID, notify, runErr)

		return false, false
	}

	hasDurableInput, pendingErr := s.hasPendingDurableInput(ctx, sessionID)
	if pendingErr != nil {
		sess.Close()
		s.reportSessionUnstarted(ctx, sessionID, notify, pendingErr)

		return false, false
	}

	recoveringAcceptedTurn := false
	if !hasDurableInput && len(inputs) == 0 && !sess.HasPendingWork() && !rs.HasRun() {
		recoveringAcceptedTurn, runErr = s.recoverableInputRunnable(ctx, sessionID)
		if runErr != nil {
			sess.Close()
			s.reportSessionUnstarted(ctx, sessionID, notify, runErr)

			return false, false
		}
	}

	hadInput := hasDurableInput || len(inputs) > 0 || recoveringAcceptedTurn

	if !hadInput && !sess.HasPendingWork() {
		sess.Close()

		if ownerlessSession(rec) {
			notify(sessionevent.Notification{Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle})
		}

		return false, false
	}

	if err := s.activateStoppedRootForScheduledTurn(ctx, rec, inputs); err != nil {
		sess.Close()
		s.reportSessionUnstarted(ctx, sessionID, notify, err)

		return false, false
	}

	rs.SetService(sess)

	s.registerScheduleTools(ctx, rec, sess)
	s.registerSubagentTools(ctx, sessionID, sess)
	s.registerMCPTools(ctx, rec, sess)
	s.registerConfigTools(ctx, rec, sess)
	s.registerSecretTool(ctx, rec, sess)
	s.registerBudgetTool(ctx, rec, sess)

	rs.MarkRun()

	runResult, runErr := s.executeSession(ctx, sess, notify)

	if rec.ParentID == 0 {
		releaseReason := "completed"
		if runErr != nil {
			releaseReason = "error"
		}

		if !runResult.Suspended || runErr != nil {
			if releaseErr := s.releaseArmedBudget(ctx, sessionID, releaseReason); releaseErr != nil {
				runErr = errors.Join(runErr, releaseErr)
			}
		}
	}

	s.reconcileLatestReadiness(ctx, sessionID)
	s.deferNotices.record(sessionID, runResult.CompactionDeferAnnounced)

	rs.SetService(nil)

	sess.Close()

	// The session's suspended state is on disk by now, so a config change it
	// staged is safe to write and restart into.
	s.runStagedApply(ctx, sessionID)

	if runErr != nil {
		s.handleRunError(ctx, sessionID, runResult.ErrorNotice, runErr, notify)

		if ownerlessSession(rec) {
			notify(sessionevent.Notification{Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle})
		}

		return false, hadInput
	}

	if runResult.Suspended {
		s.publishWaiting(ctx, sessionID, notify)
		return false, hadInput
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
	identities := make([]map[string]any, len(projections))

	for i, projection := range projections {
		identities[i] = projection.identity
	}

	identity, err := canonicalWaitingIdentities(identities)
	if err != nil {
		return fmt.Errorf("encode waiting identities: %w", err)
	}

	digest := sha256.Sum256(identity)
	hash := hex.EncodeToString(digest[:])
	// A stale waiting card is dropped without a recapture retry: the newer
	// transition that moved the generation owns the next card.
	if _, _, err := s.enqueueProgressChangeFor(ctx, sessionID, "waiting:"+hash, false); err != nil &&
		!errors.Is(err, sessionstore.ErrProgressSuperseded) && !errors.Is(err, sessionstore.ErrOutputOwner) {
		return fmt.Errorf("enqueue progress: %w", err)
	}

	return nil
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

// settleStoppedCalls is runner-owned transcript mutation used by the /stop
// lifecycle after every live writer has joined.
func (s *svc) settleStoppedCalls(ctx context.Context, sessionID int64) error {
	rec, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load stopping session %d: %w", sessionID, err)
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	if err != nil {
		return fmt.Errorf("resolve project for stopping session %d: %w", sessionID, err)
	}

	// The producer ledger is in-memory, so a stop whose second phase runs in a
	// later image owns nothing; the transcript is the only complete list.
	pending, err := s.storedExternalCalls(ctx, sessionID)
	if err != nil {
		return err
	}

	for _, call := range pending {
		s.staged.stage(sessionID, call.ID, call.Name)
	}

	sess, err := s.openSession(ctx, sessionID, workDir, rec, true, false)
	if err != nil {
		return fmt.Errorf("open stopping session %d: %w", sessionID, err)
	}
	defer sess.Close()

	if err := sess.SettleStoppedCalls(ctx, "Stopped by user."); err != nil {
		return fmt.Errorf("settle stopped calls for session %d: %w", sessionID, err)
	}

	for _, call := range pending {
		s.staged.resolve(sessionID, call.ID)
	}

	return nil
}

// closeOrphanedCalls is runner-owned transcript mutation for the boot sweep's
// PASS 0: it answers a session's unowned external calls without running its loop.
func (s *svc) closeOrphanedCalls(ctx context.Context, rec *sessionstore.SessionRecord) (int, error) {
	orphans, err := s.orphanedCalls(ctx, rec.ID)
	if err != nil || len(orphans) == 0 {
		return 0, err
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	if err != nil {
		return 0, fmt.Errorf("resolve project for session %d: %w", rec.ID, err)
	}

	// Adoption precedes construction: the session refuses to resolve a call no
	// producer ledger owns, and it snapshots that ledger when it is built.
	for _, call := range orphans {
		s.staged.stage(rec.ID, call.ID, call.Name)
	}

	// A call left uninserted returns to being an orphan and is retried next boot.
	defer func() {
		for _, call := range orphans {
			s.staged.resolve(rec.ID, call.ID)
		}
	}()

	sess, err := s.createOrResumeSession(ctx, rec.ID, workDir, rec, false)
	if err != nil {
		return 0, fmt.Errorf("open session %d to close orphaned calls: %w", rec.ID, err)
	}
	defer sess.Close()

	for _, call := range orphans {
		if _, err := sess.ResolvePendingCall(ctx, call, orphanedCallNotice(call.Name)); err != nil {
			return 0, fmt.Errorf("close orphaned call %s in session %d: %w", call.ID, rec.ID, err)
		}
	}

	return len(orphans), nil
}

func (s *svc) ensureSessionRunner(ctx context.Context, sessionID int64) error {
	if _, ok := s.runners.Load(sessionID); ok {
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

	err = s.ensureRunner(ctx, sessionID, workDir, rec.ProjectID, nil)
	if errors.Is(err, admission.ErrNoCapacity) && rec.ParentID == 0 {
		s.enqueuePendingRunner(sessionID, workDir, rec.ProjectID)
		return nil
	}

	return err
}

// reportSessionUnstarted tells the controller a session could not start and parks
// it idle. Shared so every pre-run failure reads identically to the user. A
// canceled context is a shutdown, not a session failure: restart will resume the
// work, so no error receipt is published.
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
	if outputErr := s.enqueuePersistentOutput(ctx, sessionID, message); outputErr != nil {
		logger.Ctx(ctx).Named("daemon.runner").Warn("enqueue_unstarted_error_output", zap.Error(outputErr))
	}

	notify(sessionevent.Notification{
		Type:    sessionevent.NotifyMessage,
		Message: message,
	})

	if record, err := s.sessionStore.GetSession(ctx, sessionID); err != nil || ownerlessSession(record) {
		notify(sessionevent.Notification{Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle})
	}
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
		resolution, err := sess.ResolvePendingCall(ctx, value.Call, value.Content)
		if err != nil {
			return false, fmt.Errorf("resolve %s call %s: %w", value.Call.Name, value.Call.ID, err)
		}

		s.staged.resolve(sessionID, value.Call.ID)

		return resolution == session.CallResolutionInserted, nil
	case blockingSubagentCompletionInput:
		return true, s.injectBlockingCompletion(ctx, sess, value.ChildID, value.CallID, value.ActivationSeq)
	case backgroundSubagentCompletionInput:
		return true, s.injectBackgroundCompletion(ctx, sess, value.ChildID, value.ActivationSeq)
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
	return s.openSession(ctx, sessionID, workDir, rec, false, preserveStopped)
}

// openSession builds the session service. A settlement open never persists the
// initial state: /stop settles a tree already marked stopping, and reactivating
// it would lose the lifecycle fence.
func (s *svc) openSession(
	ctx context.Context,
	sessionID int64,
	workDir string,
	rec *sessionstore.SessionRecord,
	settlement, preserveStopped bool,
) (session.Service, error) {
	externalCalls, err := s.pendingExternalCallsForSession(ctx, sessionID)
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

		ExtraSkills:         s.builtinSkillsFor(ctx, rec),
		StagedExternalCalls: externalCalls,

		CompactionDeferAnnounced: s.deferNotices.announced(sessionID),
		InputBoundary: s.inputFactory.Boundary(
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
			func(ctx context.Context, text string) (string, error) {
				return s.renderFinalOutput(ctx, sessionID, text)
			},
		),
		SettlementOpen:        settlement,
		PreserveStoppedStatus: preserveStopped,
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

	sess, err := s.factory.Create(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	return sess, nil
}

// pendingExternalCallsForSession merges the authoritative producer ledgers:
// in-memory config/secret work, the pending-apply marker that outlives it,
// durable sleep schedules, and durable blocking-child links. The transcript
// never has to infer ownership from the latest assistant turn or from a tool
// name alone.
func (s *svc) pendingExternalCallsForSession(ctx context.Context, sessionID int64) (map[string]string, error) {
	calls := s.staged.forSession(sessionID)
	if calls == nil {
		calls = make(map[string]string)
	}

	if s.applier != nil {
		owed, err := s.applier.PendingCall(sessionID)

		switch {
		case err != nil:
			logger.Ctx(ctx).Named("daemon.runner").
				Warn("read_pending_apply_marker", zap.Int64("session_id", sessionID), zap.Error(err))
		case owed.ToolCallID != "":
			calls[owed.ToolCallID] = owed.ToolName
		}
	}

	if s.scheduleSvc != nil {
		sleeps, err := s.scheduleSvc.PendingSleeps(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("load pending sleeps for session %d: %w", sessionID, err)
		}

		for _, sleep := range sleeps {
			calls[sleep.CallID] = tool.IDSleep
		}
	}

	links, err := s.links.ListPendingChildLinks(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load pending child calls for session %d: %w", sessionID, err)
	}

	for _, link := range links {
		if link.Blocking && link.TaskCallID != "" {
			calls[link.TaskCallID] = tool.IDTask
		}
	}

	return calls, nil
}

// activeSubagentInfos maps a session's pending (undelivered) child links to the
// summary the session pins in its "# Active subagents" prompt section. Empty for
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

func (s *svc) executeSession(
	ctx context.Context,
	sess session.Service,
	notify func(sessionevent.Notification),
) (session.RunResult, error) {
	notify(sessionevent.Notification{Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateRunning})

	result, runErr := sess.RunDaemon(ctx, notify)

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
		s.guardSleepWhileSubagentsPending(rec.ID, schedule.NewSleepTool(s.scheduleSvc, rec.ID)),
	)
}

// registerMCPTools registers the MCP registry tools. Root sessions only: a
// subagent must not reshape the toolset its parent will run with.
func (s *svc) registerMCPTools(ctx context.Context, rec *sessionstore.SessionRecord, sess session.Service) {
	if s.mcpStore == nil || rec.ParentID != 0 {
		return
	}

	for _, t := range newMCPTools(s.mcpStore, s.mcpPool, rec.ProjectID) {
		registerLogged(ctx, sess, t)
	}
}

// registerConfigTools registers config mutation only on the reserved system
// project. Other project roots and subagents must not reshape the daemon.
func (s *svc) registerConfigTools(ctx context.Context, rec *sessionstore.SessionRecord, sess session.Service) {
	if s.applier == nil || !s.isConfigurationSession(ctx, rec) {
		return
	}

	for _, t := range newConfigTools(s, rec.ID) {
		registerLogged(ctx, sess, t)
	}
}

// runStagedApply commits after the suspend is persisted, so a daemon that dies
// mid-apply leaves a session matching what was done. A rejected commit never
// restarts, so its verdict is delivered here.
func (s *svc) runStagedApply(ctx context.Context, sessionID int64) {
	callID, sc, ok := s.staged.takePendingApply(sessionID)
	if !ok {
		return
	}

	// A marker is armed only for a call the transcript durably carries: a verdict
	// owed to a call no session can be holding is undeliverable on every boot, and
	// the marker that outlives it turns the next boot failure into a rollback.
	backed, err := s.suspendIsDurable(ctx, sessionID, callID, sc.toolName)
	if err != nil || !backed {
		s.releaseUnbackedApply(ctx, sessionID, callID, sc, err)

		return
	}

	log := logger.Ctx(ctx).Named("daemon.apply")

	v := s.applier.Apply(sc.apply, configops.Pending{
		SessionID:  sessionID,
		ToolCallID: callID,
		ToolName:   sc.toolName,
	})

	if !v.Failed() {
		log.Info("apply_committed", zap.Int64("session_id", sessionID), zap.String("tool", sc.toolName))

		return
	}

	log.Warn("apply_rejected", zap.Int64("session_id", sessionID), zap.String("reason", v.Reason()))

	// The ledger entry stays until the rejection is durably injected — releasing
	// it here leaves the session's own result with no producer that owns it.
	if err := s.enqueueSessionInput(ctx, sessionID, pendingCallResultInput{
		Call:    session.PendingToolCall{ID: callID, Name: sc.toolName},
		Content: "Config change rejected — " + v.Reason(),
	}); err != nil {
		log.Error("rejection_delivery_failed", zap.Int64("session_id", sessionID), zap.Error(err))
	}
}

// suspendIsDurable reports whether the durable transcript carries callID as an
// unresolved external call of toolName — the precondition for committing.
func (s *svc) suspendIsDurable(ctx context.Context, sessionID int64, callID, toolName string) (bool, error) {
	calls, err := s.storedExternalCalls(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("read suspended call %s of session %d: %w", callID, sessionID, err)
	}

	return pendingCall(calls, callID, toolName), nil
}

// releaseUnbackedApply gives the slot back for a staged change whose suspend the
// transcript does not back. Nothing is written, so the change is simply dropped.
func (s *svc) releaseUnbackedApply(
	ctx context.Context,
	sessionID int64,
	callID string,
	sc stagedCall,
	scanErr error,
) {
	s.applier.ReleaseApply()

	log := logger.Ctx(ctx).Named("daemon.apply")
	log.Warn("apply_suspend_not_durable",
		zap.Int64("session_id", sessionID), zap.String("tool", sc.toolName), zap.Error(scanErr))

	// A call the transcript demonstrably lacks has nobody waiting; only an
	// unverifiable read is worth answering, since an owner-less call bricks a session.
	if scanErr == nil || s.shuttingDown.Load() {
		s.staged.resolve(sessionID, callID)

		return
	}

	if err := s.enqueueSessionInput(ctx, sessionID, pendingCallResultInput{
		Call:    session.PendingToolCall{ID: callID, Name: sc.toolName},
		Content: "Config change abandoned — the suspend could not be verified, so nothing was written.",
	}); err != nil {
		log.Error("unbacked_delivery_failed", zap.Int64("session_id", sessionID), zap.Error(err))
	}
}

// abandonStagedApply is the teardown net for a claim whose loop died before
// runStagedApply could hand it over — a panic, recovered so the daemon lives on.
// The slot is process-global: left taken, no session and no bootstrap op could
// change the config again until the daemon restarts.
func (s *svc) abandonStagedApply(ctx context.Context, sessionID int64) {
	if s.applier == nil {
		return
	}

	callID, sc, ok := s.staged.takePendingApply(sessionID)
	if !ok {
		return
	}

	s.applier.ReleaseApply()

	log := logger.Ctx(ctx).Named("daemon.apply")
	log.Warn("apply_abandoned", zap.Int64("session_id", sessionID), zap.String("tool", sc.toolName))

	// Nothing was written, so the call is answerable in-process — and the ledger
	// entry only goes away once that answer is durable.
	err := s.enqueueSessionInput(ctx, sessionID, pendingCallResultInput{
		Call:    session.PendingToolCall{ID: callID, Name: sc.toolName},
		Content: "Config change abandoned — the session ended before it was applied. Nothing was written.",
	})
	if err != nil {
		log.Error("abandon_delivery_failed", zap.Int64("session_id", sessionID), zap.Error(err))
	}
}

// registerSubagentTools registers the daemon-mode task and subagent-monitor
// tools on the session's live registry. The daemon itself is the spawner, so
// these are gated the same as schedule tools — availability still follows the
// session's agent-type allowlist.
func (s *svc) registerSubagentTools(ctx context.Context, sessionID int64, sess session.Service) {
	for _, t := range []tool.Tool{
		newTaskTool(s, sessionID, sess.AgentTypes(), s.modelCatalog),
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

	if err := s.launcher.Ensure(ctx, sessionID, workDir, projectID, inputs); err != nil {
		return fmt.Errorf("ensure session runner: %w", err)
	}

	return nil
}

func (s *svc) ensureRunnerStartable(
	ctx context.Context,
	rec *sessionstore.SessionRecord,
	inputs []queuedSessionInput,
) (bool, error) {
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

// startChildTimeout applies applyChildTimeout for runSession's setup, folding in
// the error-vs-sentinel split so the caller only branches on whether to continue.
// A genuine read failure sets *errored and returns ok=false (caller must return).
func (s *svc) startChildTimeout(
	ctx context.Context,
	rs runner,
	sessionID int64,
	errored *bool,
) (context.Context, context.CancelFunc, bool) {
	info := rs.Info()
	if info.Kind != admission.Child {
		return ctx, nil, true
	}

	timeoutCtx, cancel, err := s.applyChildTimeout(ctx, sessionID)
	if err != nil && !errors.Is(err, errNoChildTimeout) {
		// A child that never entered its loop did not complete: without this the
		// teardown finalizes it as completed once the ledger recovers.
		*errored = true

		s.reportTimeoutUnresolved(ctx, info.ParentID, sessionID, err)

		return ctx, nil, false
	}

	return timeoutCtx, cancel, true
}

// applyChildTimeout wraps ctx with a deadline for a blocking child, returning the
// derived ctx and its cancel func. errNoChildTimeout means no timeout applies
// (background child, or non-blocking link) — the caller must not treat it as a
// real failure. A read failure is a distinct, genuine error.
func (s *svc) applyChildTimeout(ctx context.Context, sessionID int64) (context.Context, context.CancelFunc, error) {
	link, err := s.links.GetLink(ctx, sessionID)
	if err != nil {
		return ctx, nil, fmt.Errorf("child timeout for session %d: %w", sessionID, err)
	}

	if link == nil || !link.Blocking {
		return ctx, nil, errNoChildTimeout
	}

	secs := link.TimeoutSec
	if secs <= 0 {
		secs = defaultBlockingTimeoutSec
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(secs)*time.Second)

	return timeoutCtx, cancel, nil
}
