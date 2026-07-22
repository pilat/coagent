package daemon

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// finalizeChild marks a subagent terminal (once its loop has fully exited and
// its final message is durably written) and delivers its completion to the
// parent. No-op for non-subagent sessions and during shutdown (the startup
// sweep re-delivers on restart). errored forces the error state (loop panic).
func (s *svc) finalizeChild(ctx context.Context, childID int64, shuttingDown, errored bool) {
	if shuttingDown {
		return
	}

	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		// Without the link the parent id is unknown, so there is nobody to notify.
		logger.Ctx(ctx).Named("daemon.completion").
			Error("finalize_get_link", zap.Int64("child", childID), zap.Error(err))

		return
	}

	if link == nil {
		return // not a subagent
	}

	if link.Terminal() {
		return // already finalized
	}

	if link.State == LinkStateStopped {
		return // /stop parks the child without producing a completion
	}

	rec, err := s.sessionStore.GetSession(ctx, childID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.completion").
			Error("finalize_get_session", zap.Int64("child", childID), zap.Error(err))
		s.notifyChildFailure(link.ParentID, childID, "could not be finalized", err)

		return
	}

	// A suspended child is waiting on an external wake (e.g. sleep), not done —
	// unless its loop panicked, which is always terminal.
	if rec.Status == sessionstore.SessionStatusSuspended && !errored {
		return
	}

	// state (lifecycle column) keeps its existing derivation so Terminal() and the
	// list queries are unchanged; outcome (the richer parent-facing signal) is
	// derived separately and may differ (e.g. a max-iterations child is
	// state=error but outcome=incomplete).
	status := LinkStateCompleted
	persistedStatus := sessionstore.SessionStatusCompleted

	if errored || rec.Status == sessionstore.SessionStatusError {
		status = LinkStateError
		persistedStatus = sessionstore.SessionStatusError
	}

	result, outcome := s.deriveChildOutcome(ctx, childID, rec.Iteration, errored)

	terminalized, err := s.finalizeActivationRetrying(ctx, childID, status, result, outcome)
	if err != nil {
		logger.Ctx(ctx).
			Named("daemon.completion").
			Error("mark_link_terminal", zap.Int64("child", childID), zap.Error(err))
		s.notifyChildFailure(link.ParentID, childID, "completion could not be recorded", err)

		return
	}

	if !terminalized {
		return
	}

	if err := s.sessionStore.UpdateSessionStatus(ctx, childID, persistedStatus); err != nil {
		logger.Ctx(ctx).
			Named("daemon.completion").
			Warn("update_child_status", zap.Int64("child", childID), zap.Error(err))
	}

	link.State = status
	link.Result = result
	link.Outcome = outcome
	s.deliverCompletionToParent(ctx, *link)
}

// deriveChildOutcome computes the parent-facing result text + outcome from the
// child's committed transcript: "error" if it crashed, "completed" if the LAST
// assistant message is a text-only answer, else "incomplete". result carries the
// answer, or a short context note when there is none.
func (s *svc) deriveChildOutcome(
	ctx context.Context,
	childID int64,
	iterations int,
	errored bool,
) (string, LinkOutcome) {
	msgs, err := s.sessionStore.LoadActiveMessages(ctx, childID)
	if err != nil {
		msgs = nil
	}

	finalText := lastStoredAssistantText(msgs)

	switch {
	case errored:
		result := finalText
		if result == "" {
			result = fmt.Sprintf("crashed after %d iterations", iterations)
		}

		return result, LinkOutcomeError
	case lastStoredMessageIsFinalAnswer(msgs):
		return finalText, LinkOutcomeCompleted
	default:
		return fmt.Sprintf("ended without a final answer after %d iterations", iterations), LinkOutcomeIncomplete
	}
}

// deliverCompletionToParent routes a completion notification to the parent,
// reviving it if idle. A killed parent rejects it (orphan policy).
func (s *svc) deliverCompletionToParent(ctx context.Context, link SubagentLink) {
	var input sessionInput
	if link.Blocking {
		input = blockingSubagentCompletionInput{
			ChildID: link.ChildID, CallID: link.TaskCallID, ActivationSeq: link.ActivationSeq,
		}
	} else {
		input = backgroundSubagentCompletionInput{ChildID: link.ChildID, ActivationSeq: link.ActivationSeq}
	}

	if err := s.enqueueSessionInput(ctx, link.ParentID, input); err != nil {
		logger.Ctx(ctx).Named("daemon.completion").Error(
			"deliver_completion_dropped",
			zap.Int64("child", link.ChildID),
			zap.Int64("parent", link.ParentID),
			zap.Error(err),
		)
	}
}

func (s *svc) injectBlockingCompletion(
	ctx context.Context,
	sess session.Service,
	childID int64,
	callID string,
	activationSeq int64,
) error {
	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		return fmt.Errorf("load blocking completion link for child %d: %w", childID, err)
	}

	if link == nil {
		return fmt.Errorf("blocking completion link for child %d not found", childID)
	}

	if link.DeliveredAt != 0 {
		return nil
	}

	if link.ActivationSeq != activationSeq {
		return nil // delayed duplicate from an earlier activation
	}

	if !link.Blocking || link.TaskCallID != callID {
		return fmt.Errorf(
			"blocking completion contract mismatch for child %d: link blocking=%t call=%q, input call=%q",
			childID,
			link.Blocking,
			link.TaskCallID,
			callID,
		)
	}

	if !pendingCall(sess.PendingExternalCalls(), callID, tool.IDTask) {
		return fmt.Errorf("blocking task call %s for child %d is not pending", callID, childID)
	}

	stored, mem, err := session.BuildBlockingSubagentCompletion(
		callID,
		s.completionContent(ctx, *link),
	)
	if err != nil {
		return fmt.Errorf("build blocking completion for child %d: %w", childID, err)
	}

	return s.persistCompletion(ctx, sess, *link, stored, mem)
}

func (s *svc) injectBackgroundCompletion(
	ctx context.Context,
	sess session.Service,
	childID int64,
	activationSeq int64,
) error {
	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		return fmt.Errorf("load background completion link for child %d: %w", childID, err)
	}

	if link == nil {
		return fmt.Errorf("background completion link for child %d not found", childID)
	}

	if link.DeliveredAt != 0 {
		return nil
	}

	if link.ActivationSeq != activationSeq {
		return nil // delayed duplicate from an earlier activation
	}

	if link.Blocking {
		return fmt.Errorf("background completion input for blocking child %d", childID)
	}

	if pending := sess.PendingExternalCalls(); len(pending) > 0 {
		return fmt.Errorf("%w: %s (%s)", errSessionInputDeferred, pending[0].ID, pending[0].Name)
	}

	stored, mem, err := session.BuildBackgroundSubagentCompletion(
		childID,
		s.completionContent(ctx, *link),
	)
	if err != nil {
		return fmt.Errorf("build background completion for child %d: %w", childID, err)
	}

	return s.persistCompletion(ctx, sess, *link, stored, mem)
}

// persistCompletion is the exactly-once commit shared by the two semantically
// distinct completion variants. The link CAS and transcript insert remain one
// transaction; only a winning commit is appended to live memory.
func (s *svc) persistCompletion(
	ctx context.Context,
	sess session.Service,
	link SubagentLink,
	stored []*sessionstore.StoredMessage,
	mem []llmwire.Message,
) error {
	msgIDs, won, err := s.sessionStore.DeliverCompletionAtomic(
		ctx, link.ParentID, stored, link.ChildID, link.ActivationSeq,
	)
	if err != nil {
		return fmt.Errorf("deliver completion for child %d: %w", link.ChildID, err)
	}

	if won {
		sess.AppendDeliveredCompletion(mem, msgIDs)
	}

	// A follow-up may have arrived while this activation was terminalizing.
	// Delivery is the ordering barrier: only after the old outcome is present in
	// the parent's transcript may the same child begin its next activation.
	return s.rearmChildAfterDelivery(context.WithoutCancel(ctx), link.ChildID)
}

func (s *svc) rearmChildAfterDelivery(ctx context.Context, childID int64) error {
	rearmed, err := s.sessionStore.RearmDeliveredSubagentWithPendingInput(ctx, childID)
	if err != nil {
		return fmt.Errorf("rearm child %d after completion delivery: %w", childID, err)
	}

	if !rearmed {
		return nil
	}

	if err := s.sessionStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusActive); err != nil {
		return fmt.Errorf("activate rearmed child %d: %w", childID, err)
	}

	if err := s.ensureSessionRunner(ctx, childID); err != nil {
		return fmt.Errorf("start rearmed child %d: %w", childID, err)
	}

	return nil
}

// injectOwedCompletions drains terminal link-ledger entries that were previously
// unable to enter the transcript because another external call was pending. It
// is called immediately after an exact call result lands, so deferral never
// relies on an in-memory notification surviving a restart.
func (s *svc) injectOwedCompletions(
	ctx context.Context,
	sess session.Service,
	parentID int64,
) error {
	links, err := s.links.ListPendingChildLinks(ctx, parentID)
	if err != nil {
		return fmt.Errorf("list owed completions for parent %d: %w", parentID, err)
	}

	for _, link := range links {
		if !link.Terminal() || !link.Blocking {
			continue
		}

		if !pendingCall(sess.PendingExternalCalls(), link.TaskCallID, tool.IDTask) {
			return fmt.Errorf(
				"terminal blocking child %d is undelivered but task call %s is not pending",
				link.ChildID,
				link.TaskCallID,
			)
		}

		if err := s.injectBlockingCompletion(
			ctx, sess, link.ChildID, link.TaskCallID, link.ActivationSeq,
		); err != nil {
			return err
		}
	}

	for _, link := range links {
		if !link.Terminal() || link.Blocking {
			continue
		}

		if _, err := s.interruptPendingSleeps(
			ctx,
			parentID,
			sess,
			"Sleep interrupted — a subagent completed.",
		); err != nil {
			return err
		}

		if pending := sess.PendingExternalCalls(); len(pending) > 0 {
			return nil
		}

		if err := s.injectBackgroundCompletion(ctx, sess, link.ChildID, link.ActivationSeq); err != nil {
			return err
		}
	}

	return nil
}

// completionContent formats a terminal child's stored result + outcome for the
// parent, via the shared formatter so it matches get_subagent_result verbatim.
func (s *svc) completionContent(ctx context.Context, link SubagentLink) string {
	res := childResult{
		ChildID:  link.ChildID,
		State:    link.State,
		Outcome:  link.Outcome,
		Output:   link.Result,
		Terminal: true,
	}

	if rec, err := s.sessionStore.GetSession(ctx, link.ChildID); err == nil {
		res.Iteration = rec.Iteration
	}

	return formatChildResult(res)
}

// Start re-establishes in-flight children and re-delivers undelivered completions
// after a restart. Only PASS 0 blocks; the resumes run asynchronously.
func (s *svc) Start(ctx context.Context) error {
	// Must precede the sweep: a session left mid-clear by the previous run would
	// otherwise be resumed in that half-torn state.
	if err := s.sessionStore.KillTerminatingSessions(ctx); err != nil {
		logger.Ctx(ctx).Named("daemon.manager").Warn("kill_terminating_sessions_failed", zap.Error(err))
	}

	// /stop is a durable two-phase park. If the process died after writing
	// stopping, finish the same idempotent operation before any recovery sweep can
	// restart work from that tree.
	records, err := s.sessionStore.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for stop recovery: %w", err)
	}

	stopping := make(map[int64]bool)

	for _, rec := range records {
		if rec.Status == sessionstore.SessionStatusStopping {
			stopping[rec.ID] = true
		}
	}

	for _, rec := range records {
		if !stopping[rec.ID] || stopping[rec.ParentID] {
			continue
		}

		if err := s.Stop(ctx, rec.ID); err != nil {
			return fmt.Errorf("recover stopping session %d: %w", rec.ID, err)
		}
	}

	// PASS 0 is the one blocking phase. Controllers and the schedule executor start
	// the moment Start returns, and a runner they open makes it skip that session.
	s.resolveOrphanedCalls(ctx)

	go s.resumeAfterRestart(context.WithoutCancel(ctx))

	return nil
}

// sweep is the whole boot recovery in order; Start splits it at the PASS 0
// boundary, which is where the blocking prefix ends.
func (s *svc) sweep(ctx context.Context) {
	s.resolveOrphanedCalls(ctx)
	s.resumeAfterRestart(ctx)
}

func (s *svc) resumeAfterRestart(ctx context.Context) {
	log := logger.Ctx(ctx).Named("daemon.sweep")

	// PASS 1 — resume children that were still running. On completion their exit
	// calls finalizeChild → deliverCompletionToParent, reviving the parent lazily.
	running, runningErr := s.links.ListRunningChildLinks(ctx)
	if runningErr != nil {
		log.Error("list_running_child_links", zap.Error(runningErr))
	}

	for _, link := range running {
		s.resumeChild(ctx, link)
	}

	// PASS 2 — re-deliver terminal-but-undelivered completions to live/idle parents.
	undelivered, undeliveredErr := s.links.ListUndeliveredParentLinks(ctx)
	if undeliveredErr != nil {
		log.Error("list_undelivered_parent_links", zap.Error(undeliveredErr))
	}

	for _, link := range undelivered {
		s.deliverCompletionToParent(ctx, link)
	}

	// PASS 3 — recover accepted normal input committed after a runner's last drain.
	resumedInput, inputErr := s.resumeSessionsWithRecoverableInput(ctx)

	counts := []zap.Field{
		zap.Int("resumed", len(running)),
		zap.Int("redelivered", len(undelivered)),
		zap.Int("input_resumed", resumedInput),
	}

	// A failed pass leaves children unresumed and completions undelivered — that
	// must never be reported with the same line as a clean recovery.
	if runningErr != nil || undeliveredErr != nil || inputErr != nil {
		log.Error("sweep_incomplete", append(counts,
			zap.Bool("running_failed", runningErr != nil),
			zap.Bool("undelivered_failed", undeliveredErr != nil),
			zap.Bool("input_failed", inputErr != nil),
		)...)

		return
	}

	log.Info("sweep_done", counts...)
}

// resumeSessionsWithRecoverableInput is sweep PASS 3: normal input either still
// queued or promoted into an unfinished user turn before the process stopped.
// Roots start immediately; children preserve the activation ordering barrier —
// running links were handled in PASS 1, terminal links rearm only once their old
// completion is delivered, and stopped links require an explicit follow-up.
func (s *svc) resumeSessionsWithRecoverableInput(ctx context.Context) (int, error) {
	log := logger.Ctx(ctx).Named("daemon.sweep")

	sessionIDs, listErr := s.inboxStore.ListSessionsWithRecoverableInput(ctx)
	if listErr != nil {
		log.Error("list_recoverable_session_input", zap.Error(listErr))

		listErr = fmt.Errorf("list sessions with recoverable input: %w", listErr)
	}

	resumed := 0

	for _, sessionID := range sessionIDs {
		link, err := s.links.GetLink(ctx, sessionID)
		if err != nil {
			log.Error("classify_recoverable_session", zap.Int64("session_id", sessionID), zap.Error(err))
			continue
		}

		switch {
		case link == nil:
			runnable, runnableErr := s.recoverableInputRunnable(ctx, sessionID)
			if runnableErr != nil {
				log.Error("classify_recoverable_root", zap.Int64("session_id", sessionID), zap.Error(runnableErr))
				continue
			}

			if !runnable {
				continue
			}

			if err := s.ensureSessionRunner(ctx, sessionID); err != nil {
				log.Error("resume_recoverable_session", zap.Int64("session_id", sessionID), zap.Error(err))
				continue
			}

			resumed++
		case link.State == LinkStateStopped:
			// Explicitly parked by /stop.
		case link.Terminal() && link.DeliveredAt != 0:
			if err := s.rearmChildAfterDelivery(ctx, sessionID); err != nil {
				log.Error("rearm_pending_child", zap.Int64("child", sessionID), zap.Error(err))
				continue
			}

			resumed++
		}
	}

	return resumed, listErr
}

// cascadeKillChildren recursively kills a session's in-flight descendants —
// blocking AND background (depth-bounded). A deliberate tree teardown (Kill/Clear)
// leaves no live receiver, so a surviving background descendant would keep
// consuming a slot and writing files while reporting to nobody. Terminal links
// (a completed-but-undelivered child included) are skipped so their stored
// result/outcome survive and they are not mislabelled killed. deadline is one
// retry budget shared by the whole walk, since Kill waits on it synchronously.
func (s *svc) cascadeKillChildren(ctx context.Context, parentID int64, depth int, deadline time.Time) {
	if depth >= maxSubagentDepth {
		return
	}

	// ListPendingChildLinks = delivered_at IS NULL, which also includes terminal
	// children whose completion was not yet delivered and queued children parked in
	// s.queue — the Terminal() guard below keeps us from re-killing the former.
	links, err := s.links.ListPendingChildLinks(ctx, parentID)
	if err != nil {
		// The walk stops here, so part of the subtree survives the teardown.
		logger.Ctx(ctx).Named("daemon.completion").
			Error("cascade_list_children", zap.Int64("parent", parentID), zap.Error(err))

		return
	}

	for _, link := range links {
		if link.Terminal() {
			continue // already done (e.g. completed-but-undelivered) — keep its result
		}

		s.cascadeKillChildren(ctx, link.ChildID, depth+1, deadline)
		s.warnKilledDescendant(ctx, link)
		s.killSubagent(ctx, link.ChildID, deadline)
	}
}

// warnKilledDescendant emits one audit line per non-terminal descendant torn down
// by a cascade kill, using only fields already in hand (no message load).
func (s *svc) warnKilledDescendant(ctx context.Context, link SubagentLink) {
	iteration := 0
	if rec, err := s.sessionStore.GetSession(ctx, link.ChildID); err == nil {
		iteration = rec.Iteration
	}

	logger.Ctx(ctx).Named("daemon.completion").Warn(
		"cascade_killed_descendant",
		zap.Int64("child", link.ChildID),
		zap.Int64("parent", link.ParentID),
		zap.String("state", string(link.State)),
		zap.Int("iteration", iteration),
	)
}

// killSubagent marks a child killed durably, then stops its runner. The terminal
// mark precedes the stop so the child's own loop teardown (finalizeChild) observes
// a terminal link and no-ops, rather than racing to deliver a stray completion.
// deadline bounds the terminal-mark retry across the whole cascade (zero = unbounded).
func (s *svc) killSubagent(ctx context.Context, childID int64, deadline time.Time) {
	// Link-terminal must commit before the status write: it is the authoritative
	// sweep signal, and MarkSessionKilled below hides a non-terminal link for good.
	err := s.markLinkTerminalRetrying(ctx, deadline, childID, LinkStateKilled, "", LinkOutcomeKilled)
	if err != nil {
		// Skipping killed_at is what keeps the child recoverable: the sweep selects
		// on `state IN ('spawned','running') AND killed_at IS NULL`.
		logger.Ctx(ctx).
			Named("daemon.completion").
			Error("kill_link_terminal", zap.Int64("child", childID), zap.Error(err))
	}

	if err == nil {
		s.markChildKilled(ctx, childID)
	}

	s.removeSchedules(ctx, childID)

	s.mu.Lock()
	rs, ok := s.loops[childID]
	s.mu.Unlock()

	if ok {
		rs.stop()
	}
}

// markChildKilled writes the session half of a kill. Called only once the link is
// terminal — the two writes together are what make the child invisible to the sweep.
func (s *svc) markChildKilled(ctx context.Context, childID int64) {
	if err := s.sessionStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusKilled); err != nil {
		logger.Ctx(ctx).
			Named("daemon.completion").
			Error("kill_child_status", zap.Int64("child", childID), zap.Error(err))
	}

	if err := s.sessionStore.MarkSessionKilled(ctx, childID); err != nil {
		logger.Ctx(ctx).
			Named("daemon.completion").
			Error("kill_child_session", zap.Int64("child", childID), zap.Error(err))
	}
}

// resumeChild restarts a child's runner so its loop can finish.
func (s *svc) resumeChild(ctx context.Context, link SubagentLink) {
	rec, err := s.sessionStore.GetSession(ctx, link.ChildID)
	if err != nil {
		return
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	if err != nil {
		return
	}

	if err := s.ensureRunner(ctx, link.ChildID, workDir, rec.ProjectID, nil); err != nil {
		logger.Ctx(ctx).
			Named("daemon.sweep").
			Error("resume_child_failed", zap.Int64("child", link.ChildID), zap.Error(err))
	}
}
