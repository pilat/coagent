package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

const interruptedCallNotice = "⚠️ This tool call was interrupted before completion (e.g., daemon restart) " +
	"and was not re-executed — the operation may have partially completed. " +
	"Check the current state before retrying."

// Start re-establishes in-flight children and re-delivers undelivered completions
// after a restart. Only PASS 0 blocks; the resumes run asynchronously.
func (s *svc) Start(ctx context.Context) error {
	if !s.life.enter() {
		return errDaemonShuttingDown
	}

	defer s.life.leave()

	// Must precede the sweep: a session left mid-clear or mid-kill by the previous
	// run would otherwise be resumed in that half-torn state.
	if err := s.finishInterruptedKills(ctx); err != nil {
		return fmt.Errorf("finish interrupted kills: %w", err)
	}

	// Finish durable stops before recovery can restart their trees.
	records, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for stop recovery: %w", err)
	}

	owedStops := make(map[int64]sessionstore.InterruptedExplicitStop)

	stops, selectErr := s.store.SelectInterruptedExplicitStops(ctx)
	if selectErr != nil {
		return fmt.Errorf("select interrupted explicit stops: %w", selectErr)
	}

	for _, stop := range stops {
		owedStops[stop.SessionID] = stop
	}

	if err := s.recoverStoppingSessions(ctx, records, owedStops); err != nil {
		return err
	}

	if err := s.convergeStoppedSessions(ctx, records, owedStops); err != nil {
		return err
	}

	if _, err := s.processes.InterruptNonterminal(ctx); err != nil {
		return fmt.Errorf("recover background process interruptions: interrupt nonterminal processes: %w", err)
	}

	if err := s.reconcileArmedBudgets(ctx); err != nil {
		return err
	}

	pending, err := s.budgets.ListPendingParks(ctx)
	if err != nil {
		return fmt.Errorf("list pending budget parks: %w", err)
	}

	for _, record := range pending {
		s.parkBudgetTree(ctx, record)
	}

	if err := s.settleUnresolvedCalls(ctx); err != nil {
		return err
	}

	s.startWake()

	if !s.life.whileOpen(func() { s.progress.Start(ctx) }) {
		return errDaemonShuttingDown
	}

	s.life.Go("daemon.recovery", s.resumeAfterRestart)

	return nil
}

func (s *svc) recoveredStopCancellationCount(
	ctx context.Context,
	rootID int64,
	since time.Time,
) (int, error) {
	count, err := s.processStore.CountTerminalByIntentSince(
		ctx, rootID, backgroundprocess.IntentSessionStopped, since,
	)
	if err != nil {
		return 0, fmt.Errorf("count stopped processes for session %d: %w", rootID, err)
	}

	return count, nil
}

// The terminating fence survives crashes, so cleanup can safely rerun before recovery.
func (s *svc) finishInterruptedKills(ctx context.Context) error {
	records, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for kill recovery: %w", err)
	}

	interrupted := make([]*sessionstore.SessionRecord, 0)

	for _, rec := range records {
		if rec.Status == sessionstore.SessionStatusTerminating && rec.KilledAt == nil {
			interrupted = append(interrupted, rec)
		}
	}

	if len(interrupted) == 0 {
		return nil
	}

	cleanupCtx := context.WithoutCancel(ctx)

	for _, record := range interrupted {
		if _, err := s.cancelSessionSubtreeProcesses(
			cleanupCtx, record.ID, backgroundprocess.IntentSessionKilled,
		); err != nil {
			return fmt.Errorf("cancel processes for interrupted kill %d: %w", record.ID, err)
		}
	}

	for _, record := range interrupted {
		cancelled, err := s.processStore.CountTerminalByIntentSince(
			cleanupCtx, record.ID, backgroundprocess.IntentSessionKilled, record.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("count processes for interrupted kill %d: %w", record.ID, err)
		}

		if _, err := s.store.MarkSessionKilledWithOutput(
			cleanupCtx, record.ID, cancelled,
		); err != nil {
			return fmt.Errorf("finish killed session %d: %w", record.ID, err)
		}

		s.removeSchedules(cleanupCtx, record.ID)
		s.killDescendants(cleanupCtx, record.ID, 0)
	}

	return nil
}

func (s *svc) recoverStoppingSessions(
	ctx context.Context,
	records []*sessionstore.SessionRecord,
	owedStops map[int64]sessionstore.InterruptedExplicitStop,
) error {
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

		if stop, owed := owedStops[rec.ID]; owed {
			if err := s.stopTreeCleanup(ctx, rec.ID, stopTreeOptions{keepRootStopping: true}); err != nil {
				return fmt.Errorf("recover stopping session %d: %w", rec.ID, err)
			}

			cancelled, err := s.recoveredStopCancellationCount(ctx, rec.ID, stop.ReceivedAt)
			if err != nil {
				return err
			}

			if err := s.completeExplicitStop(ctx, rec.ID, stop.InputID, cancelled); err != nil {
				return fmt.Errorf("recover explicit stop for session %d: %w", rec.ID, err)
			}

			continue
		}

		if err := s.stopTreeCleanup(ctx, rec.ID, stopTreeOptions{}); err != nil {
			return fmt.Errorf("recover stopping session %d: %w", rec.ID, err)
		}
	}

	return nil
}

func (s *svc) convergeStoppedSessions(
	ctx context.Context,
	records []*sessionstore.SessionRecord,
	owedStops map[int64]sessionstore.InterruptedExplicitStop,
) error {
	for _, rec := range records {
		if rec.Status != sessionstore.SessionStatusStopped {
			continue
		}

		if stop, owed := owedStops[rec.ID]; owed {
			cancelled, err := s.recoveredStopCancellationCount(ctx, rec.ID, stop.ReceivedAt)
			if err != nil {
				return err
			}

			if err := s.completeExplicitStop(ctx, rec.ID, stop.InputID, cancelled); err != nil {
				return fmt.Errorf("converge explicit stop for session %d: %w", rec.ID, err)
			}
		}
	}

	return nil
}

// orphanSweepCandidate skips lifecycles this pass does not own: /stop settles a
// parked tree from the same durable set, and killed or finished is not resumed.
func orphanSweepCandidate(rec *sessionstore.SessionRecord) bool {
	if rec.KilledAt != nil {
		return false
	}

	return rec.Status == sessionstore.SessionStatusActive ||
		rec.Status == sessionstore.SessionStatusSuspended ||
		rec.Status == sessionstore.SessionStatusError
}

func orphanedCallNotice(_ string) string {
	return "The daemon restarted while this call was out with the world, and its producer did not survive. " +
		"The outcome is unknown — check the current state before retrying."
}

func (s *svc) settleUnresolvedCalls(ctx context.Context) error {
	records, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for call recovery: %w", err)
	}

	for _, record := range records {
		if !orphanSweepCandidate(record) {
			continue
		}

		messages, err := s.store.LoadActiveMessages(ctx, record.ID)
		if err != nil {
			return fmt.Errorf("load interrupted transcript %d: %w", record.ID, err)
		}

		calls, err := session.UnresolvedStoredCalls(messages)
		if err != nil {
			return fmt.Errorf("scan interrupted transcript %d: %w", record.ID, err)
		}
		var external, internal []session.PendingToolCall

		for _, call := range calls {
			if tool.IsExternalCall(call.Name) {
				external = append(external, call)
			} else {
				internal = append(internal, call)
			}
		}

		if len(external) > 0 {
			owners, err := s.callOwners(ctx, record.ID)
			if err != nil {
				return err
			}

			for _, call := range external {
				if owners[call.ID] == call.Name {
					continue
				}

				if _, err := s.store.Enqueue(ctx, sessionstore.Input{
					SessionID: record.ID,
					Source:    sessionstore.InputSourceCallResult,
					Content: orphanedCallNotice(
						call.Name,
					),
					Attributes:  map[string]any{"call_id": call.ID, "tool_id": call.Name},
					DeliveryKey: "orphan:" + call.ID,
				}); err != nil {
					return fmt.Errorf("cancel orphaned call %s: %w", call.ID, err)
				}
			}
		}

		if len(internal) > 0 {
			if err := s.settleCalls(ctx, record.ID, internal, interruptedCallNotice, false); err != nil {
				return fmt.Errorf("settle interrupted tools %d: %w", record.ID, err)
			}
		}
	}

	return nil
}

func (s *svc) resumeAfterRestart(ctx context.Context) {
	log := logger.Ctx(ctx).Named("daemon.sweep")

	running, runningErr := s.links.ListRunningChildLinks(ctx)
	if runningErr != nil {
		log.Error("list_running_child_links", zap.Error(runningErr))
	}

	for _, link := range running {
		if err := s.start(ctx, link.ChildID); err != nil {
			logger.Ctx(ctx).
				Named("daemon.sweep").
				Error("resume_child_failed", zap.Int64("child", link.ChildID), zap.Error(err))
		}
	}

	undelivered, undeliveredErr := s.links.ListUndeliveredParentLinks(ctx)
	if undeliveredErr != nil {
		log.Error("list_undelivered_parent_links", zap.Error(undeliveredErr))
	}

	for _, link := range undelivered {
		s.deliverCompletionToParent(ctx, link)
	}

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

// Terminal children rearm only after delivery; stopped children need explicit follow-up.
func (s *svc) resumeSessionsWithRecoverableInput(ctx context.Context) (int, error) {
	log := logger.Ctx(ctx).Named("daemon.sweep")

	sessionIDs, listErr := s.store.ListSessionsWithRecoverableInput(ctx)
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
			started, resumeErr := s.resumeRecoverableRoot(ctx, sessionID)
			if resumeErr != nil {
				log.Error("resume_recoverable_session", zap.Int64("session_id", sessionID), zap.Error(resumeErr))
				continue
			}

			if started {
				resumed++
			}
		case link.State == subagent.StateStopped:
			if err := s.resumeRecoverableChild(ctx, sessionID); err != nil {
				log.Error("resume_stopped_child", zap.Int64("child", sessionID), zap.Error(err))
				continue
			}

			resumed++
		case link.State == subagent.StateError && link.DeliveredAt != 0:
			if err := s.resumeRecoverableChild(ctx, sessionID); err != nil {
				log.Error("resume_errored_child", zap.Int64("child", sessionID), zap.Error(err))
				continue
			}

			resumed++
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

func (s *svc) resumeRecoverableRoot(ctx context.Context, sessionID int64) (bool, error) {
	inputs, inputErr := s.store.ListPending(ctx, sessionID)
	if inputErr != nil {
		return false, fmt.Errorf("resume recoverable root: %w", inputErr)
	}

	for _, input := range inputs {
		if input.Source != sessionstore.InputSourceUser {
			continue
		}

		if !isEnqueueCommand(input.RawContent) {
			continue
		}

		switch strings.TrimSpace(input.RawContent) {
		case statusCommand:
			if _, err := s.handleGenericCommand(ctx, input); err != nil {
				return false, err
			}
		case stopCommand, clearCommand, killCommand:
			return s.handleGenericCommand(ctx, input)
		}
	}

	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return false, err
	}
	defer unlock()

	ctx = context.WithoutCancel(ctx)

	record, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("load recoverable root: %w", err)
	}

	runnable, err := s.recoverableRunnable(ctx, sessionID)
	if err != nil || !runnable {
		return false, err
	}

	if record.Status == sessionstore.SessionStatusStopped {
		preserveStopped, preserveErr := s.commandOnlyStoppedRoot(ctx, record)
		if preserveErr != nil {
			return false, preserveErr
		}

		if !preserveStopped {
			if err := s.store.UpdateSessionStatus(ctx, sessionID, sessionstore.SessionStatusActive); err != nil {
				return false, fmt.Errorf("resume stopped root: %w", err)
			}
		}
	}

	if err := s.startLocked(ctx, sessionID); err != nil {
		return false, err
	}

	return true, nil
}

func (s *svc) resumeRecoverableChild(ctx context.Context, childID int64) error {
	unlock, err := s.lockSessionTree(ctx, childID)
	if err != nil {
		return err
	}
	defer unlock()

	ctx = context.WithoutCancel(ctx)

	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		return fmt.Errorf("reload recoverable child link: %w", err)
	}

	if link == nil || (link.State != subagent.StateStopped &&
		(link.State != subagent.StateError || link.DeliveredAt == 0)) {
		return nil
	}

	return s.resumeChildWithPendingInputLocked(ctx, childID)
}
