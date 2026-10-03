package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// keepRootStopping leaves the terminal status and visible output to the caller's transaction.
type stopTreeOptions struct {
	keepRootStopping            bool
	preserveBackgroundProcesses bool
	cancelledProcesses          *int
}

type tree struct {
	rootID   int64
	byID     map[int64]*sessionstore.SessionRecord
	children map[int64][]int64
}

type treeLocks struct{ locks sync.Map }

type sessionTreeLock struct {
	token chan struct{}
}

type stopPlan struct {
	rootID     int64
	sessionIDs []int64
	links      []subagent.Link
}

func (s *svc) kill(ctx context.Context, input *sessionstore.InboxInput) error {
	unlock, err := s.lockSessionTree(ctx, input.SessionID)
	if err != nil {
		return err
	}
	defer unlock()

	if err := s.handleLifecycleInput(ctx, input, "Stopping session..."); err != nil {
		return err
	}

	return s.killLocked(ctx, input.SessionID)
}

// The already-stopped check and receipt share the fence with tree cleanup.
func (s *svc) stop(ctx context.Context, input *sessionstore.InboxInput) error {
	unlock, err := s.lockSessionTree(ctx, input.SessionID)
	if err != nil {
		return err
	}
	defer unlock()

	record, err := s.store.GetSession(ctx, input.SessionID)
	if err != nil {
		return fmt.Errorf("load stop session: %w", err)
	}

	if record.Status == sessionstore.SessionStatusStopped {
		return s.handleStoppedStop(ctx, input)
	}

	if err := s.handleLifecycleInput(ctx, input, "⏳ Stopping…"); err != nil {
		return err
	}

	return s.stopLocked(ctx, input.SessionID, input.ID)
}

func (s *svc) Fence(fenceCtx context.Context, rootSessionID int64) (func(), error) {
	unlock, err := s.lockSessionTree(fenceCtx, rootSessionID)
	if err != nil {
		return nil, fmt.Errorf("lock process tree %d: %w", rootSessionID, err)
	}

	if s.life.closed() {
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

func (s *svc) killLocked(ctx context.Context, sessionID int64) error {
	rs, ok := s.runners.load(sessionID)

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

	cancelledProcesses, err := s.cancelSessionSubtreeProcesses(
		cleanupCtx, sessionID, backgroundprocess.IntentSessionKilled,
	)
	if err != nil {
		return fmt.Errorf("cancel background processes: %w", err)
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

	s.killDescendants(cleanupCtx, sessionID, 0)

	if ownerlessSession(rec) {
		s.publish(sessionID, sessionevent.Notification{
			Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle, Reason: "killed",
		})
	}

	return nil
}

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

// A committed start receipt needs a terminal output even if ownership lookup failed.

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

//nolint:wsl_v5 // The second stop phase belongs beside the public Stop transition.
func (s *svc) stopTreeCleanup(ctx context.Context, sessionID int64, options stopTreeOptions) error {
	cleanupCtx := context.WithoutCancel(ctx)

	plan, err := s.beginStop(cleanupCtx, sessionID)
	if err != nil {
		return fmt.Errorf("begin stop tree: %w", err)
	}

	ids := plan.SessionIDs()

	s.runners.forget(ids)

	runners := make([]*runner, 0, len(ids))
	for _, id := range ids {
		rs, _ := s.runners.load(id)

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
		if _, err := s.schedules.CancelPendingSleeps(cleanupCtx, id); err != nil {
			return fmt.Errorf("cancel one-shot waits for session %d: %w", id, err)
		}
	}
	return s.cancelStopInputs(cleanupCtx, plan)
}

func (s *svc) stopTreeBackgroundProcesses(ctx context.Context, sessionID int64, options stopTreeOptions) error {
	if options.preserveBackgroundProcesses {
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

// Settling after writers join prevents replaying tool calls whose producers stopped.

func (s *svc) settleStoppedTree(ctx context.Context, ids []int64) error {
	for _, id := range ids {
		if err := s.commitStoppedSession(ctx, id); err != nil {
			return err
		}
	}

	return nil
}

func (s *svc) clear(ctx context.Context, input *sessionstore.InboxInput) (int64, error) {
	unlock, err := s.lockSessionTree(ctx, input.SessionID)
	if err != nil {
		return 0, err
	}
	defer unlock()

	return s.clearLocked(ctx, input.SessionID, input.ID)
}

func (s *svc) clearLocked(ctx context.Context, sessionID, inputID int64) (int64, error) {
	log := logger.Ctx(ctx).Named("manager.clear")

	s.routes.claim.Lock()
	defer s.routes.claim.Unlock()

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
	if err := s.schedules.RemoveAllForSession(ctx, sessionID); err != nil {
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

func (s *svc) lockSessionTree(ctx context.Context, sessionID int64) (func(), error) {
	record, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load session tree lock: %w", err)
	}

	return s.trees.lock(ctx, sessionRootID(record))
}

// commitStoppedSession is runner-owned transcript mutation used by the /stop
// lifecycle after every live writer has joined.
func (s *svc) commitStoppedSession(ctx context.Context, sessionID int64) error {
	messages, err := s.store.LoadActiveMessages(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("settle stopped transcript: %w", err)
	}

	calls, err := session.UnresolvedStoredCalls(messages)
	if err != nil {
		return fmt.Errorf("settle stopped transcript: %w", err)
	}

	return s.settleCalls(ctx, sessionID, calls, "Stopped by user.", true)
}

func (s *svc) settleCalls(ctx context.Context, sessionID int64, calls []session.PendingToolCall,
	notice string, expireActivation bool,
) error {
	commit := sessionstore.Commit{
		SessionID: sessionID, Mode: sessionstore.CommitLifecycle,
		ToolResults: session.SettleResults(calls, notice),
	}
	if expireActivation {
		activation, err := s.store.PendingActivation(ctx, sessionID)
		if err != nil && !errors.Is(err, sessionstore.ErrActivationNotFound) {
			return fmt.Errorf("settle activation: %w", err)
		}

		if activation != nil {
			commit.Activation = &sessionstore.ActivationChange{
				InputID: activation.InputID,
				State:   sessionstore.ActivationExpired,
			}
		}
	}

	if _, err := s.store.Commit(ctx, commit); err != nil {
		return fmt.Errorf("settle calls: %w", err)
	}

	return nil
}

func (s *svc) loadTree(ctx context.Context, sessionID int64) (*tree, error) {
	rec, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load tree session: %w", err)
	}

	rootID := sessionRootID(rec)

	records, err := s.store.ListTree(ctx, rootID)
	if err != nil {
		return nil, fmt.Errorf("list session tree: %w", err)
	}

	t := &tree{
		rootID:   rootID,
		byID:     make(map[int64]*sessionstore.SessionRecord, len(records)),
		children: make(map[int64][]int64),
	}
	for _, record := range records {
		t.byID[record.ID] = record
		if record.ParentID != 0 {
			t.children[record.ParentID] = append(t.children[record.ParentID], record.ID)
		}
	}

	return t, nil
}

func (t *tree) subtree(id int64) ([]int64, error) {
	if t.byID[id] == nil {
		return nil, fmt.Errorf("session %d not found", id)
	}

	ids := []int64{id}
	seen := map[int64]bool{id: true}

	for pos := 0; pos < len(ids); pos++ {
		for _, childID := range t.children[ids[pos]] {
			if seen[childID] {
				continue
			}

			seen[childID] = true
			ids = append(ids, childID)
		}
	}

	return ids, nil
}

func (t *tree) stopIDs() []int64 {
	ids := []int64{t.rootID}
	walk := []int64{t.rootID}

	seen := map[int64]bool{t.rootID: true}
	for pos := 0; pos < len(walk); pos++ {
		for _, childID := range t.children[walk[pos]] {
			child := t.byID[childID]
			if seen[childID] || child.KilledAt != nil {
				continue
			}

			seen[childID] = true

			walk = append(walk, childID)
			if stopActive(child.Status) {
				ids = append(ids, childID)
			}
		}
	}

	return ids
}

func (s *svc) liveRunners(t *tree) []*runner {
	ids := make([]int64, 0, len(t.byID))
	for id := range t.byID {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	var runners []*runner
	for _, id := range ids {
		if r, ok := s.runners.load(id); ok {
			runners = append(runners, r)
		}
	}

	return runners
}

func (t *treeLocks) lock(ctx context.Context, rootID int64) (func(), error) {
	value, _ := t.locks.LoadOrStore(rootID, newSessionTreeLock())

	lock, ok := value.(*sessionTreeLock)
	if !ok {
		return nil, fmt.Errorf("invalid session tree lock for root %d", rootID)
	}

	return lock.acquire(ctx)
}

func (p *stopPlan) SessionIDs() []int64 {
	return append([]int64(nil), p.sessionIDs...)
}

func (s *svc) beginStop(ctx context.Context, rootID int64) (*stopPlan, error) {
	plan, err := s.stopPlan(ctx, rootID)
	if err != nil {
		return nil, err
	}

	for _, id := range plan.sessionIDs {
		if err := s.store.UpdateSessionStatus(ctx, id, sessionstore.SessionStatusStopping); err != nil {
			return nil, fmt.Errorf("mark session %d stopping: %w", id, err)
		}
	}

	for _, link := range plan.links {
		if err := s.links.MarkLinkStopped(ctx, link.ChildID); err != nil {
			return nil, fmt.Errorf("mark subagent %d stopped: %w", link.ChildID, err)
		}
	}

	return plan, nil
}

func (s *svc) cancelStopInputs(ctx context.Context, plan *stopPlan) error {
	if _, err := s.store.CancelPendingInputsForStop(ctx, plan.sessionIDs, "stopped"); err != nil {
		return fmt.Errorf("cancel stopped session input: %w", err)
	}

	return nil
}

func (s *svc) finishStop(ctx context.Context, plan *stopPlan, keepRootStopping bool) error {
	for _, link := range plan.links {
		if err := s.links.MakeStoppedLinkResumable(ctx, link.ChildID); err != nil {
			return fmt.Errorf("detach stopped subagent %d: %w", link.ChildID, err)
		}
	}

	for _, id := range plan.sessionIDs {
		if keepRootStopping && id == plan.rootID {
			continue
		}

		if err := s.store.UpdateSessionStatus(ctx, id, sessionstore.SessionStatusStopped); err != nil {
			return fmt.Errorf("mark session %d stopped: %w", id, err)
		}
	}

	return nil
}

func (s *svc) completeExplicitStop(
	ctx context.Context,
	rootID, inputID int64,
	cancelledProcesses int,
) error {
	if _, err := s.store.CompleteExplicitStop(ctx, rootID, inputID, cancelledProcesses); err != nil {
		return fmt.Errorf("commit explicit stop completion: %w", err)
	}

	if _, err := s.store.ReactivateForSchedule(ctx, rootID); err != nil {
		return fmt.Errorf("complete explicit stop: %w", err)
	}

	record, err := s.store.GetSession(ctx, rootID)
	if err == nil {
		owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
		if owner != "" {
			_, _ = s.store.WakeOutputHead(ctx, owner)
		}
	}

	return nil
}

func (s *svc) stopPlan(ctx context.Context, rootID int64) (*stopPlan, error) {
	t, err := s.loadTree(ctx, rootID)
	if err != nil {
		return nil, err
	}

	t.rootID = rootID
	ids := t.stopIDs()

	included := make(map[int64]bool, len(ids))
	for _, id := range ids {
		included[id] = true
	}

	for _, r := range s.liveRunners(t) {
		record := t.byID[r.sessionID]
		if record.KilledAt != nil || included[record.ID] || (record.ID != rootID && record.RootID != rootID) {
			continue
		}

		ids = append(ids, record.ID)
		included[record.ID] = true
	}

	links := make([]subagent.Link, 0, len(ids))
	for _, id := range ids {
		link, linkErr := s.links.GetLink(ctx, id)
		if linkErr != nil {
			return nil, fmt.Errorf("load subagent link for session %d: %w", id, linkErr)
		}

		if link != nil && !link.Terminal() {
			links = append(links, *link)
		}
	}

	return &stopPlan{rootID: rootID, sessionIDs: ids, links: links}, nil
}

func stopActive(status sessionstore.SessionStatus) bool {
	return status == sessionstore.SessionStatusActive ||
		status == sessionstore.SessionStatusSuspended ||
		status == sessionstore.SessionStatusStopping ||
		status == sessionstore.SessionStatusStopped
}

func (s *svc) cancelSessionSubtreeProcesses(
	ctx context.Context,
	sessionID int64,
	intent backgroundprocess.HostIntent,
) (int, error) {
	t, err := s.loadTree(ctx, sessionID)
	if err != nil {
		return 0, err
	}

	sessionIDs, err := t.subtree(sessionID)
	if err != nil {
		return 0, err
	}

	cancelled, err := s.processes.CancelSessions(ctx, sessionIDs, intent)
	if err != nil {
		return cancelled, fmt.Errorf("cancel subtree processes: %w", err)
	}

	return cancelled, nil
}

func (s *svc) retireTreeToolResources(ctx context.Context, sessionID int64) error {
	t, err := s.loadTree(ctx, sessionID)
	if err != nil {
		return err
	}

	ids, err := t.subtree(sessionID)
	if err != nil {
		return err
	}

	for _, id := range ids {
		if err := sessionbuild.RetireToolResources(s.build.Resources, id); err != nil {
			return fmt.Errorf("retire session %d tools: %w", id, err)
		}
	}

	return nil
}
