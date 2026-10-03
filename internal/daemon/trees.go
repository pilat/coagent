package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

// stopTreeCleanup durably parks a tree without publishing user-command events.
// Startup recovery and budget parking use it to finish an interrupted stop
// without replaying UI. With keepRootStopping the explicit root stays in its
// stopping fence: the caller owns the single terminal transaction that moves it
// to `stopped` together with the visible completion output.
type stopTreeOptions struct {
	keepRootStopping            bool
	preserveBackgroundProcesses bool
	cancelledProcesses          *int
}

type sessionTreeLock struct {
	token chan struct{}
}

func (s *svc) Kill(ctx context.Context, sessionID int64) error {
	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return err
	}
	defer unlock()

	return s.killLocked(ctx, sessionID)
}

// Stop parks a session tree without destroying it. Every active descendant is
// stopped, one-shot waits and pending external calls receive an explicit stopped
// result, and accepted-but-unconsumed input is cancelled. Recurring schedules
// remain installed. A later root message resumes only the root; a stopped child
// requires an explicit send_to_subagent follow-up.
//
// An explicit manager-owned /stop (inputID > 0) leaves its root in `stopping`
// after cleanup and commits the durable terminal output in one transaction with
// the budget release and the final stopped status. A failure before that
// commit leaves the root stopping and publishes no success.
func (s *svc) Stop(ctx context.Context, sessionID, inputID int64) error {
	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return err
	}
	defer unlock()

	return s.stopLocked(ctx, sessionID, inputID)
}

func (s *svc) Clear(ctx context.Context, sessionID int64) (int64, error) {
	return s.clear(ctx, sessionID, 0)
}

func (s *svc) Fence(fenceCtx context.Context, rootSessionID int64) (func(), error) {
	unlock, err := s.lockSessionTree(fenceCtx, rootSessionID)
	if err != nil {
		return nil, fmt.Errorf("lock process tree %d: %w", rootSessionID, err)
	}

	if s.shuttingDown.Load() {
		unlock()

		return nil, backgroundprocess.ErrFenced
	}

	rec, err := s.store.GetSession(fenceCtx, rootSessionID)
	if err != nil {
		unlock()

		if errors.Is(err, sql.ErrNoRows) {
			return nil, backgroundprocess.ErrFenced
		}

		return nil, fmt.Errorf("fence session %d lookup: %w", rootSessionID, err)
	}

	if rec.Status == sessionstore.SessionStatusStopping ||
		rec.Status == sessionstore.SessionStatusTerminating ||
		rec.KilledAt != nil {
		unlock()

		return nil, backgroundprocess.ErrFenced
	}

	return unlock, nil
}

//nolint:funcorder // Public lifecycle methods delegate into the shared tree lock.
func (s *svc) killLocked(ctx context.Context, sessionID int64) error {
	rs, ok := s.runners.Load(sessionID)

	if ok {
		s.publish(
			sessionID,
			sessionevent.Notification{
				Type:    sessionevent.NotifyMessage,
				Message: "Stopping session...",
			},
		)

		rs.Stop()
	}

	rec, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session %d not found", sessionID)
	}

	if rec.KilledAt != nil {
		return s.retireTreeToolResources(ctx, sessionID)
	}

	if rec.ParentID == 0 {
		if err := s.releaseArmedBudget(ctx, sessionID, "killed"); err != nil {
			return err
		}
	}

	// Cleanup must complete even if the caller disconnects mid-Kill — detach
	// from request-scoped cancellation while keeping logger values.
	cleanupCtx := context.WithoutCancel(ctx)

	cancelledProcesses := 0
	if s.processSvc != nil {
		cancelledProcesses, err = s.cancelSessionSubtreeProcesses(
			cleanupCtx, sessionID, backgroundprocess.IntentSessionKilled,
		)
		if err != nil {
			return fmt.Errorf("cancel background processes: %w", err)
		}
	}

	if _, err := s.store.MarkSessionKilledWithOutput(
		cleanupCtx, sessionID, cancelledProcesses,
	); err != nil {
		return fmt.Errorf("mark session killed with output: %w", err)
	}

	if err := s.retireTreeToolResources(cleanupCtx, sessionID); err != nil {
		return err
	}

	s.removeSchedules(cleanupCtx, sessionID)

	// Cascade-kill every non-terminal descendant (blocking and background): this is
	// a deliberate tree teardown, so background work that would outlive it and
	// report to nobody is stopped too. Completed-but-undelivered children keep their
	// result (see cascadeKillChildren).
	s.cascadeKillChildrenForKilledTree(
		cleanupCtx, sessionID, 0, time.Now().Add(cascadeRetryBudget),
	)

	if ownerlessSession(rec) {
		s.publish(sessionID, sessionevent.Notification{
			Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle, Reason: "killed",
		})
	}

	return nil
}

//nolint:funcorder // Public lifecycle methods delegate into the shared tree lock.
func (s *svc) stopLocked(ctx context.Context, sessionID, inputID int64) error {
	record, getErr := s.store.GetSession(ctx, sessionID)
	if getErr != nil {
		// Fail closed: an unread session must not be classified as ownerless,
		// so the idle publication is skipped instead of faked.
		logger.Ctx(ctx).Warn("stop_session_lookup_failed",
			zap.Int64("session_id", sessionID), zap.Error(getErr))

		record = nil
	}

	explicit := inputID > 0 && record != nil && !ownerlessSession(record)

	if !explicit {
		s.publish(sessionID, sessionevent.Notification{
			Type:    sessionevent.NotifyMessage,
			Message: "⏹ Stopping...",
		})
	}

	cancelledProcesses := 0
	if err := s.stopTreeCleanup(ctx, sessionID, stopTreeOptions{
		keepRootStopping: explicit, cancelledProcesses: &cancelledProcesses,
	}); err != nil {
		return err
	}

	if explicit {
		return s.completeExplicitStop(ctx, sessionID, inputID, cancelledProcesses)
	}

	if err := s.releaseArmedBudget(ctx, sessionID, "stopped"); err != nil {
		return err
	}

	if err := s.convergeOrphanedStopStart(ctx, sessionID, inputID, cancelledProcesses); err != nil {
		return err
	}

	if record != nil && ownerlessSession(record) {
		s.publish(sessionID, sessionevent.Notification{
			Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle, Reason: "stopped",
		})
	}

	return nil
}

// convergeOrphanedStopStart finishes the terminal fact when the stop fence
// committed a start row before the ownership check could classify the stop.
// Without it a replaceable "Stopping…" receipt would stay dangling with no
// recovery path, because startup only converges roots still in `stopping`.
//
//nolint:funcorder // completes the stop transition documented above.
func (s *svc) convergeOrphanedStopStart(
	ctx context.Context,
	sessionID, inputID int64,
	cancelledProcesses int,
) error {
	if inputID <= 0 {
		return nil
	}

	record, recordErr := s.store.GetSession(ctx, sessionID)
	// Ownerless stops have no start row to converge; an unread session stays a
	// startup-recovery case rather than a terminal fact published blind.
	if recordErr == nil && !ownerlessSession(record) {
		return s.completeExplicitStop(ctx, sessionID, inputID, cancelledProcesses)
	}

	return nil
}

//nolint:funcorder,wsl_v5 // The second stop phase belongs beside the public Stop transition.
func (s *svc) stopTreeCleanup(ctx context.Context, sessionID int64, options stopTreeOptions) error {
	cleanupCtx := context.WithoutCancel(ctx)

	liveSessionIDs, err := s.liveTreeRunnerIDs(cleanupCtx, sessionID)
	if err != nil {
		return err
	}
	plan, err := s.beginStop(cleanupCtx, sessionID, liveSessionIDs)
	if err != nil {
		return fmt.Errorf("begin stop tree: %w", err)
	}

	ids := plan.SessionIDs()

	s.removeQueuedSessions(ids)

	runners := make([]*runner, 0, len(ids))
	for _, id := range ids {
		rs, _ := s.runners.Load(id)

		if rs != nil {
			runners = append(runners, rs)
		}
	}

	// Signal the entire tree before waiting for any one runner: a foreground
	// parent can otherwise keep a child alive while stop is waiting on it.
	for _, rs := range runners {
		rs.Cancel()
	}

	for _, rs := range runners {
		<-rs.Done()
	}
	if err := s.retireTreeToolResources(cleanupCtx, sessionID); err != nil {
		return err
	}

	if err := s.settleStoppedTree(cleanupCtx, ids); err != nil {
		return err
	}

	if err := s.finishStop(cleanupCtx, plan, options.keepRootStopping); err != nil {
		return fmt.Errorf("finish stop tree: %w", err)
	}

	if !options.keepRootStopping {
		if err := s.releaseArmedBudget(cleanupCtx, sessionID, "stopped"); err != nil {
			return err
		}
	}
	if err := s.stopTreeBackgroundProcesses(cleanupCtx, sessionID, options); err != nil {
		return err
	}
	for _, id := range ids {
		if s.scheduleSvc != nil {
			if _, err := s.scheduleSvc.CancelPendingSleeps(cleanupCtx, id); err != nil {
				return fmt.Errorf("cancel one-shot waits for session %d: %w", id, err)
			}
		}
	}
	if err := s.cancelStopInputs(cleanupCtx, plan); err != nil {
		return err
	}

	return nil
}

//nolint:funcorder // Background cancellation is a phase of the adjacent stop transition.
func (s *svc) stopTreeBackgroundProcesses(ctx context.Context, sessionID int64, options stopTreeOptions) error {
	if s.processSvc == nil || options.preserveBackgroundProcesses {
		return nil
	}

	cancelled, err := s.cancelSessionSubtreeProcesses(ctx, sessionID, backgroundprocess.IntentSessionStopped)
	if err != nil {
		return fmt.Errorf("cancel background processes: %w", err)
	}

	if options.cancelledProcesses != nil {
		*options.cancelledProcesses = cancelled
	}

	return nil
}

// settleStoppedTree closes every outstanding tool_use once all writers have
// joined. That is what makes a stopped session resumable without replaying a
// sleep/config/task call that no longer exists.
//
//nolint:funcorder // Stop-tree helpers stay beside the stop they serve.
func (s *svc) settleStoppedTree(ctx context.Context, ids []int64) error {
	for _, id := range ids {
		if err := s.commitStoppedSession(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

//nolint:funcorder,wsl_v5 // Runner discovery must immediately precede stop planning.
func (s *svc) liveTreeRunnerIDs(ctx context.Context, rootID int64) ([]int64, error) {
	records, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list live tree runners: %w", err)
	}

	var ids []int64
	for _, record := range records {
		if record.ID != rootID && record.RootID != rootID {
			continue
		}
		if _, ok := s.runners.Load(record.ID); ok {
			ids = append(ids, record.ID)
		}
	}

	return ids, nil
}

//nolint:funcorder // Clear's command variant shares one replacement transaction with Clear.
func (s *svc) clear(ctx context.Context, sessionID, inputID int64) (int64, error) {
	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	defer unlock()

	return s.clearLocked(ctx, sessionID, inputID)
}

//nolint:funcorder // Public lifecycle methods delegate into the shared tree lock.
func (s *svc) clearLocked(ctx context.Context, sessionID, inputID int64) (int64, error) {
	log := logger.Ctx(ctx).Named("manager.clear")

	s.routeMu.Lock()
	defer s.routeMu.Unlock()

	rec, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("session %d not found", sessionID)
	}

	if rec.KilledAt != nil {
		return 0, fmt.Errorf("session %d is already killed", sessionID)
	}

	workDir, _ := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	projectName, _ := s.store.GetProjectName(ctx, rec.ProjectID)
	owner, _ := rec.Attributes[controllerapi.SessionAttributeManagerID].(string)
	var newRec *sessionstore.SessionRecord

	//nolint:nestif // Owner-aware replacement is the one boundary that preserves a manager surface.
	if owner != "" {
		if inputID > 0 {
			newRec, _, err = s.store.ReplaceManagerRootForInput(ctx, sessionID, inputID, projectName, workDir)
		} else {
			newRec, _, err = s.store.ReplaceManagerRoot(ctx, sessionID, projectName, workDir)
		}

		if err != nil {
			return 0, fmt.Errorf("replace manager session: %w", err)
		}
	} else {
		newRec, err = s.store.CreateReplacementSession(ctx, sessionID)
		if err != nil {
			return 0, fmt.Errorf("create replacement session: %w", err)
		}
	}

	name := fmt.Sprintf("%s - %d", projectName, newRec.ID)
	s.publish(sessionID, sessionevent.Notification{
		Type:         sessionevent.NotifySessionCleared,
		OldSessionID: sessionID,
		NewSessionID: newRec.ID,
		Name:         name,
		WorkDir:      workDir,
		Attributes:   rec.Attributes,
	})

	if err := s.killLocked(ctx, sessionID); err != nil {
		log.Warn("clear_kill_old_session_failed", zap.Int64("session_id", sessionID), zap.Error(err))
	}

	return newRec.ID, nil
}

// newProcessService shares the tree lock between process admission and stop.
func (s *svc) newProcessService(ctx context.Context) backgroundprocess.Service {

	outputRoot, err := coagenthome.Join(coagenthome.ProcessesDirName)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.process").Warn("process_output_dir", zap.Error(err))

		return backgroundprocess.NewService(s.processStore, backgroundprocess.Options{
			Fence: s,
		})
	}

	return backgroundprocess.NewService(s.processStore, backgroundprocess.Options{
		OutputDir: outputRoot,
		Fence:     s,
	})
}

func (s *svc) removeSchedules(ctx context.Context, sessionID int64) {
	if s.scheduleSvc == nil {
		return
	}

	if err := s.scheduleSvc.RemoveAllForSession(ctx, sessionID); err != nil {
		logger.Ctx(ctx).Named("daemon.manager").
			Warn("remove_schedules_failed", zap.Int64("session_id", sessionID), zap.Error(err))
	}
}

func newSessionTreeLock() *sessionTreeLock {
	lock := &sessionTreeLock{token: make(chan struct{}, 1)}
	lock.token <- struct{}{}

	return lock
}

func (l *sessionTreeLock) acquire(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("acquire session tree lock: %w", ctx.Err())
	case <-l.token:
		return func() { l.token <- struct{}{} }, nil
	}
}

//nolint:wsl_v5 // Resolution and acquisition must remain one keyed-lock operation.
func (s *svc) lockSessionTree(ctx context.Context, sessionID int64) (func(), error) {
	record, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load session tree lock: %w", err)
	}

	rootID := sessionRootID(record)
	value, _ := s.treeLocks.LoadOrStore(rootID, newSessionTreeLock())
	lock, ok := value.(*sessionTreeLock)
	if !ok {
		return nil, fmt.Errorf("invalid session tree lock for root %d", rootID)
	}

	return lock.acquire(ctx)
}
