package daemon

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

// A stale runnability projection must not spin runners without new input.
const maxEmptyLoopIterations = 3

type runOutcome struct {
	again, hadInput, errored, publishIdle bool
}

func (s *svc) runSession(ctx context.Context, rs *runner) {
	defer s.life.leave()

	var last runOutcome
	defer func() { s.finishRunner(ctx, rs, last, recover()) }()

	rec, err := s.store.GetSession(ctx, rs.sessionID)
	if err != nil {
		return
	}

	s.announceSession(ctx, rs, rec)

	emptyRuns := 0

	for {
		last = s.runOnce(ctx, rs)
		if !last.again {
			return
		}

		if last.hadInput {
			emptyRuns = 0
			continue
		}

		emptyRuns++
		if emptyRuns >= maxEmptyLoopIterations {
			logger.Ctx(ctx).
				Named("daemon.runner").
				Error("session_loop_spin_guard", zap.Int64("session_id", rs.sessionID), zap.Int("empty_runs", emptyRuns))

			return
		}
	}
}

func (s *svc) runOnce(ctx context.Context, rs *runner) runOutcome {
	id := rs.sessionID

	rec, err := s.store.GetSession(ctx, id)
	if err != nil {
		return runOutcome{}
	}

	idle := ownerlessSession(rec)

	sess, cleanup, err := s.openRunner(ctx, rs)
	if err != nil {
		s.reportSessionUnstarted(ctx, id, err)
		return runOutcome{errored: true, publishIdle: idle}
	}
	defer cleanup()
	defer sess.Close()
	defer func() { rs.SetService(nil); s.updateLive(context.WithoutCancel(ctx), id) }()

	rows, err := s.store.ListPending(ctx, id)
	if err != nil {
		s.reportSessionUnstarted(ctx, id, err)
		return runOutcome{errored: true}
	}

	pending, err := s.pendingRunnable(ctx, id, rows)
	if err != nil {
		s.reportSessionUnstarted(ctx, id, err)
		return runOutcome{errored: true}
	}

	if !pending && !sess.HasPendingWork() && settledActivation(rec, rs.HasRun()) {
		return runOutcome{publishIdle: idle}
	}

	if rec.ParentID == 0 {
		s.progress.Wake()
	}

	rs.MarkRun()
	s.publish(id, sessionevent.Notification{Type: sessionevent.NotifyStateChanged, Status: sessionevent.StateRunning})

	result, runErr := sess.Run(ctx)
	runErr = s.settleRunBudget(ctx, rec, result, runErr)
	s.closeRunner(ctx, rs, rec, result, sess, cleanup)

	if runErr == nil && result.Suspended {
		runErr = s.applySuspendedConfig(ctx, id)
	}

	return s.concludeRun(ctx, rs, result, runErr, runOutcome{hadInput: pending}, idle)
}

func (s *svc) openRunner(ctx context.Context, rs *runner) (*session.Session, func(), error) {
	if err := s.applier.SettleStagedResults(ctx, rs.sessionID); err != nil {
		return nil, nil, fmt.Errorf("settle runner configuration results: %w", err)
	}

	unlock, err := s.lockSessionTree(ctx, rs.sessionID)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()

	rec, err := s.store.GetSession(ctx, rs.sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("load session before construction: %w", err)
	}

	sess, cleanup, err := s.openSession(ctx, rs.sessionID, rs.workDir, rec, rs.PreserveStopped())
	if err != nil {
		return nil, nil, err
	}

	rs.SetService(sess)
	s.updateLive(ctx, rs.sessionID)

	return sess, cleanup, nil
}

func (s *svc) closeRunner(ctx context.Context, rs *runner, rec *sessionstore.SessionRecord,
	result session.RunResult, sess *session.Session, cleanup func(),
) {
	s.progress.ReconcileLatestReadiness(ctx, rs.sessionID)
	s.runners.recordDefer(rs.sessionID, result.DeferNoticeAnnounced)
	rs.SetService(nil)
	s.updateLive(ctx, rs.sessionID)

	if rec.ParentID == 0 {
		s.progress.Wake()
	}

	sess.Close()
	cleanup()
}

func (s *svc) concludeRun(ctx context.Context, rs *runner, result session.RunResult,
	runErr error, out runOutcome, idle bool,
) runOutcome {
	if runErr != nil {
		s.handleRunError(ctx, rs.sessionID, result.ErrorNotice, runErr)

		out.errored, out.publishIdle = true, idle

		return out
	}

	if result.BudgetFired {
		return out
	}

	if result.Suspended {
		s.publishWaiting(ctx, rs.sessionID)
		return out
	}

	if rs.PreserveStopped() {
		rec, err := s.store.GetSession(ctx, rs.sessionID)
		if err != nil || rec.Status == sessionstore.SessionStatusStopped {
			return out
		}
	}

	out.again = true

	return out
}

func (s *svc) finishRunner(ctx context.Context, rs *runner, out runOutcome, panicValue any) {
	id := rs.sessionID
	closing := s.life.closed()

	errored := out.errored || panicValue != nil || (ctx.Err() != nil && !closing)
	if panicValue != nil {
		logger.Ctx(ctx).
			Named("daemon.runner").
			Error("session_panic", zap.Int64("session_id", id), zap.Any("panic", panicValue))
	}

	cleanup := context.WithoutCancel(ctx)
	if !closing {
		s.applier.Abandon(cleanup, id)
	}

	released := ctx.Err() != nil || closing
	if released {
		s.releaseRunner(cleanup, rs)
	}

	if closing {
		return
	}

	unlock, releasedNow, err := s.finishFence(ctx, cleanup, rs, released)
	if releasedNow {
		released = true

		if !s.life.closed() {
			errored = true
		}
	}

	if err != nil {
		logger.Ctx(cleanup).Named("daemon.runner").Error("finish_runner_fence_failed", zap.Error(err))
	}

	if !released {
		s.removeRunner(cleanup, rs)
	}

	s.finishRunnerState(cleanup, rs, released)
	var deliver func()

	if err == nil {
		deliver = s.finishRunnerDelivery(ctx, rs, out, errored)
	}

	if unlock != nil {
		unlock()
	}

	if deliver != nil {
		deliver()
	}

	if !s.life.closed() {
		s.drain(cleanup)

		if ctx.Err() == nil && !errored {
			s.restartPendingAfterExit(cleanup, id)
		}
	}
}

func (s *svc) finishRunnerState(ctx context.Context, rs *runner, released bool) {
	if rs.PreserveStopped() {
		s.preserveRunnerStopped(ctx, rs.sessionID)
	}

	if !released {
		rs.Complete()
	}
}

func (s *svc) finishRunnerDelivery(ctx context.Context, rs *runner, out runOutcome, errored bool) func() {
	cleanup := context.WithoutCancel(ctx)
	id := rs.sessionID

	if err := s.applier.SettleStagedResults(cleanup, id); err != nil {
		logger.Ctx(cleanup).Named("daemon.apply").Error("abandon_delivery_failed", zap.Error(err))
	}

	deliver := s.finalizeChildLocked(cleanup, id, errored)
	s.progress.ReconcileLatestReadiness(cleanup, id)

	if out.publishIdle && ctx.Err() == nil {
		s.publishOwnerlessIdle(cleanup, id)
	}

	return deliver
}

func (s *svc) finishFence(ctx, cleanup context.Context, rs *runner, released bool) (func(), bool, error) {
	if released {
		unlock, err := s.lockSessionTree(cleanup, rs.sessionID)
		return unlock, false, err
	}

	unlock, err := s.lockSessionTree(ctx, rs.sessionID)
	if err == nil || ctx.Err() == nil {
		return unlock, false, err
	}
	// Stop holds the tree fence while joining, so release the join before waiting again.
	s.releaseRunner(cleanup, rs)
	unlock, err = s.lockSessionTree(cleanup, rs.sessionID)

	return unlock, true, err
}

func (s *svc) announceSession(ctx context.Context, rs *runner, rec *sessionstore.SessionRecord) {
	projectName, _ := s.store.GetProjectName(ctx, rs.projectID)
	s.publish(rs.sessionID, sessionevent.Notification{
		Type: sessionevent.NotifySessionCreated,
		Name: fmt.Sprintf("%s - %d", projectName, rs.sessionID), WorkDir: rs.workDir, Attributes: rec.Attributes,
	})
}

func (s *svc) restartPendingAfterExit(ctx context.Context, sessionID int64) {
	link, err := s.links.GetLink(ctx, sessionID)
	if err != nil {
		return
	}

	rows, err := s.store.ListPending(ctx, sessionID)
	if err != nil {
		return
	}

	runnable, err := s.pendingRunnable(ctx, sessionID, rows)
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

	if err := s.start(ctx, sessionID); err != nil {
		logger.Ctx(ctx).Named("daemon.runner").Error(
			"restart_pending_session", zap.Int64("session_id", sessionID), zap.Error(err))
	}
}

// Failed starts preserve input; only the first failure for that work is reported.
// Shutdown cancellation leaves recovery to the next boot without an error receipt.
func (s *svc) reportSessionUnstarted(
	ctx context.Context,
	sessionID int64,
	err error,
) {
	if ctx.Err() != nil {
		return
	}

	message := fmt.Sprintf(
		"⚠️ Session error: %s\n\nThe session is still alive — send a message to retry.",
		logger.Redact(err.Error()),
	)

	reported, outputErr := s.store.RecordSessionStartFailure(ctx, sessionID, message)
	if outputErr != nil {
		logger.Ctx(ctx).Named("daemon.runner").Warn("enqueue_unstarted_error_output", zap.Error(outputErr))
		return
	}

	if !reported {
		return
	}

	s.publish(sessionID, sessionevent.Notification{
		Type:    sessionevent.NotifyMessage,
		Message: message,
	})
}

func (s *svc) publishOwnerlessIdle(ctx context.Context, sessionID int64) {
	if s.HasActiveLoop(sessionID) {
		return
	}

	record, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.runner").Warn(
			"idle_owner_lookup_failed", zap.Int64("session_id", sessionID), zap.Error(err),
		)

		return
	}

	if !ownerlessSession(record) {
		return
	}

	s.publish(sessionID, sessionevent.Notification{
		Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle,
	})
}

// The caller is responsible for closing the session before calling this.
func (s *svc) handleRunError(
	ctx context.Context,
	sessionID int64,
	message string,
	runErr error,
) {
	if ctx.Err() != nil {
		// Shutdown — don't flush or notify (sessions will be resumed on restart).
		return
	}

	logger.Ctx(ctx).Warn("session_error", zap.Int64("session_id", sessionID), zap.Error(runErr))

	if message != "" {
		return
	}

	if message == "" {
		message = fmt.Sprintf(
			"⚠️ Session error: %s\n\nThe session is still alive — send a message to continue.",
			logger.Redact(runErr.Error()),
		)
	}

	s.publish(sessionID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: message})
}

func (s *svc) preserveRunnerStopped(cleanup context.Context, sessionID int64) {
	record, loadErr := s.store.GetSession(cleanup, sessionID)
	if loadErr == nil && record.Status == sessionstore.SessionStatusActive {
		if statusErr := s.store.UpdateSessionStatus(
			cleanup,
			sessionID,
			sessionstore.SessionStatusStopped,
		); statusErr != nil {
			logger.Ctx(cleanup).Named("daemon.runner").Error("preserve_stopped_status", zap.Error(statusErr))
		}
	}
}

func settledActivation(rec *sessionstore.SessionRecord, hasRun bool) bool {
	return hasRun || rec.CompletionCheckConfirmedAnswerID == nil
}

func (s *svc) applySuspendedConfig(ctx context.Context, id int64) error {
	unlock, err := s.lockSessionTree(ctx, id)
	if err != nil {
		return err
	}

	s.applier.RunStagedApply(ctx, id)
	unlock()

	return nil
}
