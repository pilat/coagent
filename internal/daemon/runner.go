package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

// A stale runnability projection must not spin runners without new input.
const maxEmptyLoopIterations = 3

type waitingProjection struct {
	wait     sessionevent.WaitItem
	display  map[string]any
	identity map[string]any
}

// runSession is the main goroutine for a session.
// ctx is already cancelable through rs (set in ensureRunner).
func (s *svc) runSession(ctx context.Context, sessionID int64, rs *runner) {
	errored := false
	publishIdle := false

	defer func() {
		s.finishRunner(ctx, sessionID, rs, &errored, publishIdle, recover())
	}()

	notify := func(n sessionevent.Notification) {
		s.publish(sessionID, n)
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
	rs *runner,
	errored *bool,
	publishIdle bool,
	panicValue any,
) {
	shuttingDown := s.shuttingDown.Load()
	if panicValue != nil {
		*errored = true
		logger.Ctx(ctx).
			Named("daemon.runner").
			Error("session_panic", zap.Int64("session_id", sessionID), zap.Any("panic", panicValue))
	}
	if ctx.Err() != nil && !shuttingDown {
		*errored = true
	}
	cleanup := context.WithoutCancel(ctx)
	if !shuttingDown {
		s.applier.Abandon(cleanup, sessionID)
	}
	cancelled := ctx.Err() != nil
	if cancelled || shuttingDown {
		s.completeCancelledRunner(cleanup, sessionID, rs)
	}
	if shuttingDown {
		return
	}
	unlock, err := s.acquireFinishFence(ctx, cleanup, sessionID, rs, &cancelled, errored)
	if err != nil {
		logger.Ctx(cleanup).Named("daemon.runner").Error("finish_runner_fence_failed", zap.Error(err))
	}
	if !cancelled {
		s.removeRunner(cleanup, sessionID)
		info := rs.Info()
		s.admit.release(info.Child, info.ParentID)
	}
	info := rs.Info()
	s.preserveRunnerStopped(cleanup, sessionID, info.PreserveStopped, shuttingDown)

	if !cancelled {
		rs.Complete()
	}
	if err == nil {
		s.settleRunnerApply(cleanup, sessionID)
	}

	var deliver func()
	if !shuttingDown && err == nil {
		deliver = s.finalizeChildLocked(cleanup, sessionID, false, *errored)
		s.progress.ReconcileLatestReadiness(cleanup, sessionID)
		s.publishOwnerlessIdleAfterTeardown(cleanup, sessionID, publishIdle, false, ctx.Err() != nil, false)
	}
	if unlock != nil {
		unlock()
	}
	if deliver != nil {
		deliver()
	}
	if !shuttingDown && ctx.Err() == nil {
		s.drainPendingRunners(cleanup)
		s.drainQueue(cleanup)
		if !*errored {
			s.restartPendingAfterExit(cleanup, sessionID)
		}
	}
}

func (s *svc) completeCancelledRunner(ctx context.Context, sessionID int64, rs *runner) {
	s.removeRunner(ctx, sessionID)

	info := rs.Info()
	s.admit.release(info.Child, info.ParentID)
	rs.Complete()
}

func (s *svc) acquireFinishFence(
	ctx, cleanup context.Context,
	sessionID int64,
	rs *runner,
	cancelled, errored *bool,
) (func(), error) {
	if *cancelled {
		return s.lockSessionTree(cleanup, sessionID)
	}

	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err == nil || ctx.Err() == nil {
		return unlock, err
	}

	// Stop holds this fence while joining the runner, so cancellation must release the join first.
	*cancelled = true

	if !s.shuttingDown.Load() {
		*errored = true
	}

	s.completeCancelledRunner(cleanup, sessionID, rs)

	return s.lockSessionTree(cleanup, sessionID)
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

func (s *svc) runSessionIteration(
	ctx context.Context,
	sessionID int64,
	rs *runner,
	notify func(sessionevent.Notification),
	announced, publishIdle, errored *bool,
) (bool, bool) {
	rec, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return false, false
	}

	s.announceRunnerOnce(ctx, sessionID, rs, rec, notify, announced)

	info := rs.Info()
	idleEligible := ownerlessSession(rec)

	sess, cleanup, err := s.prepareRunnerActivation(ctx, sessionID, info.WorkDir, rs, info.PreserveStopped)
	if err != nil {
		*errored = true
		*publishIdle = idleEligible

		s.reportSessionUnstarted(ctx, sessionID, notify, err)

		return false, false
	}
	defer cleanup()
	defer sess.Close()
	defer s.releaseRunnerService(ctx, sessionID, rs)

	pending, err := s.pendingInputRunnable(ctx, sessionID)
	if err != nil {
		*errored = true

		s.reportSessionUnstarted(ctx, sessionID, notify, err)

		return false, false
	}

	hadInput := pending
	if !pending && !sess.HasPendingWork() {
		if settledActivation(rec, rs.HasRun()) {
			*publishIdle = idleEligible
			return false, false
		}
	}

	s.wakeRootProgress(rec.ParentID)

	rs.MarkRun()
	notify(sessionevent.Notification{Type: sessionevent.NotifyStateChanged, Status: sessionevent.StateRunning})

	result, runErr := sess.Run(ctx)
	runErr = s.settleRunBudget(ctx, rec, result, runErr, notify)

	s.releaseActivationResources(ctx, sessionID, rs, result, sess, cleanup, rec.ParentID)

	if runErr == nil && result.Suspended {
		runErr = s.applySuspendedConfig(ctx, sessionID)
	}

	return s.continueActivation(
		ctx,
		sessionID,
		result,
		runErr,
		notify,
		errored,
		publishIdle,
		idleEligible,
		info.PreserveStopped,
	), hadInput
}

func (s *svc) continueActivation(
	ctx context.Context,
	sessionID int64,
	result session.RunResult,
	runErr error,
	notify func(sessionevent.Notification),
	errored, publishIdle *bool,
	idleEligible, preserveStopped bool,
) bool {
	if runErr != nil {
		*errored = true
		*publishIdle = idleEligible

		s.handleRunError(ctx, sessionID, result.ErrorNotice, runErr, notify)

		return false
	}

	if result.BudgetFired {
		return false
	}

	if result.Suspended {
		s.publishWaiting(ctx, sessionID, notify)
		return false
	}

	if preserveStopped {
		record, loadErr := s.store.GetSession(ctx, sessionID)
		if loadErr != nil || record.Status == sessionstore.SessionStatusStopped {
			return false
		}
	}

	return true
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
	if _, _, err := s.progress.EnqueueChangeFor(ctx, sessionID, causalID, false); err != nil &&
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

// commitStoppedSession is runner-owned transcript mutation used by the /stop
// lifecycle after every live writer has joined.
func (s *svc) commitStoppedSession(ctx context.Context, sessionID int64) error {
	messages, err := s.store.LoadActiveMessages(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("settle stopped transcript: %w", err)
	}

	wire, err := storedWireMessages(messages)
	if err != nil {
		return fmt.Errorf("settle stopped transcript: %w", err)
	}

	calls := session.UnresolvedCalls(wire)

	activation, err := s.store.PendingActivation(ctx, sessionID)
	if err != nil && !errors.Is(err, sessionstore.ErrActivationNotFound) {
		return fmt.Errorf("settle stopped transcript: %w", err)
	}

	commit := sessionstore.Commit{
		SessionID:   sessionID,
		Mode:        sessionstore.CommitLifecycle,
		ToolResults: session.SettleResults(calls, "Stopped by user."),
	}
	if activation != nil {
		commit.Activation = &sessionstore.ActivationChange{
			InputID: activation.InputID,
			State:   sessionstore.ActivationExpired,
		}
	}

	_, err = s.store.Commit(ctx, commit)
	if err != nil {
		return fmt.Errorf("settle stopped transcript: %w", err)
	}

	return nil
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
	if _, ok := s.runners.Load(sessionID); ok {
		return nil
	}

	rec, err := s.store.GetSession(ctx, sessionID)
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

	err = s.ensureRunnerLocked(ctx, sessionID, workDir, rec.ProjectID)
	if errors.Is(err, errNoCapacity) && rec.ParentID == 0 {
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

	reported, outputErr := s.store.RecordSessionStartFailure(ctx, sessionID, message)
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

func (s *svc) announceSession(
	ctx context.Context,
	sessionID int64,
	rs *runner,
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

func (s *svc) createOrResumeSession(
	ctx context.Context,
	sessionID int64,
	workDir string,
	rs *runner,
	preserveStopped bool,
) (*session.Session, func(), error) {
	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()

	rec, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("load session before construction: %w", err)
	}

	sess, cleanup, err := s.openSession(ctx, sessionID, workDir, rec, preserveStopped)
	if err != nil {
		return nil, nil, err
	}

	rs.SetService(sess)
	s.updateLive(ctx, sessionID)

	return sess, cleanup, nil
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

func (s *svc) openSession(
	ctx context.Context,
	sessionID int64,
	workDir string,
	rec *sessionstore.SessionRecord,
	preserveStopped bool,
) (*session.Session, func(), error) {
	externalCalls, err := s.pendingExternalCallsForSession(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}

	repoRoot, err := s.sessionRepoRoot(ctx, rec)
	if err != nil {
		return nil, nil, err
	}

	in := s.buildInput
	in.Record, in.WorkDir, in.RepoRoot = rec, workDir, repoRoot
	in.ExternalCalls = externalCalls
	in.Events = &sessionEvents{daemon: s, sessionID: sessionID}
	in.CompactionDeferAnnounced = s.deferNotices.announced(sessionID)
	in.PreserveStoppedStatus = preserveStopped
	in.Loader = loader.New(in.MarketplaceCache)

	schedules, schedulesErr := s.scheduleSvc.Render(ctx, sessionID)
	if schedulesErr != nil {
		return nil, nil, fmt.Errorf("capture construction schedules: %w", schedulesErr)
	}

	in.Schedules = schedules

	status, statusErr := s.progress.Current(ctx, sessionRootID(rec))
	if statusErr != nil {
		return nil, nil, fmt.Errorf("capture construction status: %w", statusErr)
	}

	in.Status = status.Rendered

	owner, _ := rec.Attributes[controllerapi.SessionAttributeManagerID].(string)
	in.OutputEnabled = rec.ParentID == 0 && owner != ""
	in.ActiveSubagents = s.activeSubagentInfos(ctx, sessionID)
	in.ActiveProcesses = s.activeProcessInfos(ctx, sessionID)

	// Subagents never carry the instruction: the attribute marks roots only,
	// and a resumed root reattaches it through this same open path.
	if rec.ParentID == 0 && isManagementSurface(rec.Attributes) {
		skill, err := loader.BuiltinSkill(loader.ManagementSkillName)
		if err != nil {
			return nil, nil, fmt.Errorf("load management skill: %w", err)
		}

		in.ExtraSkills = append(in.ExtraSkills, skill)
	}

	in.OwnerTools = s.ownerTools(rec, in.Loader)

	sess, cleanup, err := sessionbuild.Build(ctx, in)
	if err != nil {
		return nil, nil, fmt.Errorf("assemble session: %w", err)
	}

	return sess, cleanup, nil
}

// sessionRepoRoot follows the durable tree root because child session rows do
// not copy manager-owned attributes from their parent.
func (s *svc) sessionRepoRoot(ctx context.Context, rec *sessionstore.SessionRecord) (string, error) {
	if rec.RootID == 0 {
		repoRoot, _ := rec.Attributes["repo_root"].(string)

		return repoRoot, nil
	}

	root, err := s.store.GetSession(ctx, rec.RootID)
	if err != nil {
		return "", fmt.Errorf("load root session %d for worktree policy: %w", rec.RootID, err)
	}

	if root == nil || root.ProjectID != rec.ProjectID {
		return "", fmt.Errorf("root session %d does not match project %d", rec.RootID, rec.ProjectID)
	}

	repoRoot, _ := root.Attributes["repo_root"].(string)

	return repoRoot, nil
}

// Ownership comes from producer ledgers and exact queued results, never tool names alone.
func (s *svc) pendingExternalCallsForSession(ctx context.Context, sessionID int64) (map[string]string, error) {
	stored, err := s.store.LoadActiveMessages(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load pending external calls: %w", err)
	}

	unresolved, err := unresolvedStoredCalls(stored, func(string) bool { return true })
	if err != nil {
		return nil, fmt.Errorf("load pending external calls: %w", err)
	}

	actual := make(map[string]string, len(unresolved))
	for _, call := range unresolved {
		actual[call.ID] = call.Name
	}

	calls := s.applier.Calls(sessionID)

	if calls == nil {
		calls = make(map[string]string)
	}

	owed, err := s.applier.PendingCall(sessionID)

	switch {
	case err != nil:
		logger.Ctx(ctx).Named("daemon.runner").
			Warn("read_pending_apply_marker", zap.Int64("session_id", sessionID), zap.Error(err))
	case owed.ToolCallID != "":
		calls[owed.ToolCallID] = owed.ToolName
	}

	sleeps, err := s.scheduleSvc.PendingSleeps(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load pending sleeps for session %d: %w", sessionID, err)
	}

	for _, sleep := range sleeps {
		calls[sleep.CallID] = tool.IDSleep
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

	// Producer delivery atomically trades its ledger claim for a queued result.
	if err := s.pendingResultOwners(ctx, sessionID, actual, calls); err != nil {
		return nil, fmt.Errorf("load pending external calls: %w", err)
	}

	for callID, name := range calls {
		if actual[callID] != name {
			delete(calls, callID)
		}
	}

	return calls, nil
}

// activeSubagentInfos maps a session's pending (undelivered) child links to the
// summary the session pins in its active-background prompt section. Empty for
// a leaf session with no children.
func (s *svc) activeSubagentInfos(ctx context.Context, sessionID int64) []sessionprompt.ActiveSubagentInfo {
	links, err := s.links.ListPendingChildLinks(ctx, sessionID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.runner").
			Warn("list_pending_child_links", zap.Int64("session_id", sessionID), zap.Error(err))

		return nil
	}

	if len(links) == 0 {
		return nil
	}

	infos := make([]sessionprompt.ActiveSubagentInfo, 0, len(links))
	for _, l := range links {
		infos = append(infos, sessionprompt.ActiveSubagentInfo{
			ChildID:  l.ChildID,
			Blocking: l.Blocking,
			State:    string(l.State),
		})
	}

	return infos
}

func (s *svc) activeProcessInfos(ctx context.Context, sessionID int64) []sessionprompt.ActiveProcessInfo {
	processes, err := s.processStore.ListRunningBySessions(ctx, []int64{sessionID})
	if err != nil {
		logger.Ctx(ctx).Named("daemon.runner").
			Warn("list_running_processes", zap.Int64("session_id", sessionID), zap.Error(err))

		return nil
	}

	infos := make([]sessionprompt.ActiveProcessInfo, 0, len(processes))
	for _, process := range processes {
		if process.AdvertisedAt == nil {
			continue
		}

		infos = append(infos, sessionprompt.ActiveProcessInfo{ID: process.ID, OutputPath: process.OutputPath})
	}

	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })

	return infos
}

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

	if message != "" {
		return
	}

	if message == "" {
		message = fmt.Sprintf(
			"⚠️ Session error: %s\n\nThe session is still alive — send a message to continue.",
			logger.Redact(runErr.Error()),
		)
	}

	notify(sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: message})
}

func (s *svc) ownerTools(rec *sessionstore.SessionRecord, ldr loader.Service) []tool.Tool {
	tools := []tool.Tool{
		subagent.NewTaskTool(subagent.Spawner(s), rec.ID, ldr, s.modelCatalog),
		subagent.NewGetSubagentResultTool(subagent.Spawner(s)), subagent.NewSendToSubagentTool(subagent.Spawner(s)),
	}

	if rec.ParentID == 0 {
		tools = append(tools, schedule.NewScheduleTool(rec.ID, s.scheduleSvc, time.Local))
	}

	tools = append(tools, s.scheduleSvc.SleepTool(rec.ID))

	if rec.ParentID == 0 {
		tools = append(tools, mcpstore.NewTools(s.mcpStore, rec.ProjectID)...)

		tools = append(tools,
			configapply.NewConfigEdit(rec.ID, s.applier),
			budget.NewTool(budget.Store(s.store), rec.ID, s.modelHasPricing(rec.Model)),
		)
	}

	return tools
}

func (s *svc) ensureRunner(
	ctx context.Context,
	sessionID int64,
	workDir string,
	projectID int64,
) error {
	if s.shuttingDown.Load() {
		return errDaemonShuttingDown
	}

	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return err
	}
	defer unlock()

	return s.ensureRunnerLocked(ctx, sessionID, workDir, projectID)
}

func (s *svc) ensureRunnerLocked(
	ctx context.Context,
	sessionID int64,
	workDir string,
	projectID int64,
) error {
	if s.shuttingDown.Load() {
		return errDaemonShuttingDown
	}

	if err := s.launchRunner(ctx, sessionID, workDir, projectID); err != nil {
		return fmt.Errorf("ensure session runner: %w", err)
	}

	return nil
}

func (s *svc) ensureRunnerStartable(
	ctx context.Context,
	rec *sessionstore.SessionRecord,
) (bool, error) {
	preserveStopped, err := s.commandOnlyStoppedRoot(ctx, rec)
	if err != nil {
		return false, err
	}

	if err := validateRunnerStart(rec, preserveStopped); err != nil {
		return false, err
	}

	return preserveStopped, nil
}

func validateRunnerStart(rec *sessionstore.SessionRecord, preserveStopped bool) error {
	if rec.KilledAt != nil || rec.Status == sessionstore.SessionStatusKilled {
		return fmt.Errorf("session %d is killed", rec.ID)
	}

	if rec.Status == sessionstore.SessionStatusStopping || rec.Status == sessionstore.SessionStatusTerminating ||
		(rec.Status == sessionstore.SessionStatusStopped && !preserveStopped) {
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

	head, err := s.store.PeekPending(ctx, rec.ID)
	if err != nil {
		return false, fmt.Errorf("peek stopped root input: %w", err)
	}

	return isReadOnlyBoundaryCommand(head.RawContent), nil
}

func (s *svc) preserveRunnerStopped(cleanup context.Context, sessionID int64, preserve, shuttingDown bool) {
	if !preserve || shuttingDown {
		return
	}

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

func (s *svc) settleRunnerApply(cleanup context.Context, sessionID int64) {
	if settleErr := s.applier.SettleStagedResults(cleanup, sessionID); settleErr != nil {
		logger.Ctx(cleanup).Named("daemon.apply").Error("abandon_delivery_failed", zap.Error(settleErr))
	}
}

func settledActivation(rec *sessionstore.SessionRecord, hasRun bool) bool {
	return hasRun || rec.CompletionCheckConfirmedAnswerID == nil
}

func (s *svc) settleRunBudget(
	ctx context.Context,
	rec *sessionstore.SessionRecord,
	result session.RunResult,
	runErr error,
	notify func(sessionevent.Notification),
) error {
	if result.BudgetFired {
		record, budgetErr := s.budgetSvc.Get(ctx, sessionRootID(rec))
		if budgetErr != nil {
			runErr = errors.Join(runErr, budgetErr)
		} else if record.ParkPhase == budgetParkRequested {
			s.startBudgetPark(record)
		}
	}

	if rec.ParentID == 0 && !result.BudgetFired {
		runErr = errors.Join(runErr, s.settleRootBudget(ctx, rec.ID, result.Suspended, runErr, notify))
	}

	return runErr
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

func (s *svc) pendingResultOwners(ctx context.Context, sessionID int64, actual, calls map[string]string) error {
	pending, err := s.store.ListPending(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load pending result owners: %w", err)
	}

	for _, row := range pending {
		if row.Source == sessionstore.InputSourceCallResult {
			callID, _ := row.Attributes["call_id"].(string)

			toolID, _ := row.Attributes["tool_id"].(string)
			if actual[callID] == toolID && toolID != "" {
				calls[callID] = toolID
			}
		}
	}

	return nil
}

func (s *svc) wakeRootProgress(parentID int64) {
	if parentID == 0 {
		s.progress.Wake()
	}
}

func (s *svc) announceRunnerOnce(
	ctx context.Context,
	id int64,
	rs *runner,
	rec *sessionstore.SessionRecord,
	notify func(sessionevent.Notification),
	announced *bool,
) {
	if *announced {
		return
	}

	*announced = true

	s.announceSession(ctx, id, rs, rec, notify)
}

func (s *svc) releaseRunnerService(ctx context.Context, id int64, rs *runner) {
	rs.SetService(nil)
	s.updateLive(context.WithoutCancel(ctx), id)
}

func (s *svc) prepareRunnerActivation(
	ctx context.Context,
	id int64,
	workDir string,
	rs *runner,
	preserveStopped bool,
) (*session.Session, func(), error) {
	if err := s.applier.SettleStagedResults(ctx, id); err != nil {
		return nil, nil, fmt.Errorf("settle runner configuration results: %w", err)
	}

	return s.createOrResumeSession(ctx, id, workDir, rs, preserveStopped)
}

func (s *svc) releaseActivationResources(
	ctx context.Context,
	sessionID int64,
	rs *runner,
	result session.RunResult,
	sess *session.Session,
	cleanup func(),
	parentID int64,
) {
	s.progress.ReconcileLatestReadiness(ctx, sessionID)
	s.deferNotices.record(sessionID, result.DeferNoticeAnnounced)
	rs.SetService(nil)
	s.updateLive(ctx, sessionID)

	s.wakeRootProgress(parentID)

	sess.Close()
	cleanup()
}
