package daemon

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

const (
	completionAttempts = 3
	completionBackoff  = 150 * time.Millisecond
)

func (s *svc) finalizeChildLocked(ctx context.Context, childID int64, shuttingDown, errored bool) func() {
	if shuttingDown {
		return nil
	}

	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.completion").Error(
			"finalize_get_link", zap.Int64("child", childID), zap.Error(err),
		)

		return nil
	}

	if link == nil || link.Terminal() || link.State == subagent.StateStopped {
		return nil
	}

	record, err := s.store.GetSession(ctx, childID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.completion").Error(
			"finalize_get_session", zap.Int64("child", childID), zap.Error(err),
		)
		s.notifyChildFailure(ctx, link.ParentID, childID, "could not be finalized", err)

		return nil
	}

	if record.Status == sessionstore.SessionStatusSuspended && !errored {
		return nil
	}

	// A child that ended through the terminal empty-stop notice completed
	// successfully regardless of how the runner observed its exit: the
	// durable streak is the recovery evidence, so its link stays completed.
	terminalEmptyStop := record.EmptyStopStreak >= sessionstore.EmptyStopTerminalStreak

	state := subagent.StateCompleted
	persistedStatus := sessionstore.SessionStatusCompleted

	if (errored || record.Status == sessionstore.SessionStatusError) && !terminalEmptyStop {
		state = subagent.StateError
		persistedStatus = sessionstore.SessionStatusError
	}

	result, outcome := s.deriveOutcome(
		ctx, childID, record.Iteration, errored, record.Status == sessionstore.SessionStatusError,
		record.EmptyStopStreak,
	)

	terminalized, err := s.finalizeActivation(ctx, childID, state, result, outcome)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.completion").Error(
			"mark_link_terminal", zap.Int64("child", childID), zap.Error(err),
		)
		s.notifyChildFailure(ctx, link.ParentID, childID, "completion could not be recorded", err)

		return nil
	}

	if !terminalized {
		return nil
	}

	if err := s.store.UpdateSessionStatus(ctx, childID, persistedStatus); err != nil {
		logger.Ctx(ctx).Named("daemon.completion").Warn(
			"update_child_status", zap.Int64("child", childID), zap.Error(err),
		)
	}

	link.State = state
	link.Result = result
	link.Outcome = outcome

	s.publishSubagentProgress(ctx, childID)

	return func() { s.deliverCompletionToParent(ctx, *link) }
}

func (s *svc) rearmChildAfterDelivery(ctx context.Context, childID int64) error {
	unlock, err := s.lockSessionTree(ctx, childID)
	if err != nil {
		return err
	}
	defer unlock()

	return s.rearm(context.WithoutCancel(ctx), childID)
}

func (s *svc) rearm(ctx context.Context, childID int64) error {
	rearmed, err := s.subagents.RearmDeliveredWithPendingInput(ctx, childID)
	if err != nil {
		return fmt.Errorf("rearm child %d after completion delivery: %w", childID, err)
	}

	if !rearmed {
		return nil
	}

	if err := s.store.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusActive); err != nil {
		return fmt.Errorf("activate rearmed child %d: %w", childID, err)
	}

	s.publishSubagentProgress(ctx, childID)

	if err := s.ensureSessionRunnerLocked(ctx, childID); err != nil {
		return fmt.Errorf("start rearmed child %d: %w", childID, err)
	}

	return nil
}

func (s *svc) finalizeActivation(
	ctx context.Context,
	childID int64,
	state subagent.State,
	result string,
	outcome subagent.Outcome,
) (bool, error) {
	var err error

	for attempt := range completionAttempts {
		if attempt > 0 {
			time.Sleep(completionBackoff)
		}

		var finalized bool

		finalized, err = s.subagents.TryFinalizeActivation(ctx, childID, state, result, outcome)
		if err == nil {
			return finalized, nil
		}
	}

	return false, fmt.Errorf("finalize activation for child %d: %w", childID, err)
}

func (s *svc) guardChildTransition(
	ctx context.Context,
	childID int64,
	transition func(context.Context) error,
) error {
	unlock, err := s.lockSessionTree(ctx, childID)
	if err != nil {
		return err
	}
	defer unlock()

	return transition(context.WithoutCancel(ctx))
}

// finalizeChild marks a subagent terminal (once its loop has fully exited and
// its final message is durably written) and delivers its completion to the
// parent. No-op for non-subagent sessions. errored forces the error state.
func (s *svc) finalizeChild(ctx context.Context, childID int64) {
	var deliver func()

	err := s.guardChildTransition(ctx, childID, func(guarded context.Context) error {
		deliver = s.finalizeChildLocked(guarded, childID, false, false)

		return nil
	})
	if err != nil {
		logger.Ctx(ctx).Named("daemon.completion").Error(
			"finalize_child_fence_failed", zap.Int64("child", childID), zap.Error(err),
		)

		return
	}

	if deliver != nil {
		deliver()
	}
}

// deliverCompletionToParent routes a completion notification to the parent,
// reviving it if idle. A killed parent rejects it (orphan policy).
func (s *svc) deliverCompletionToParent(ctx context.Context, link subagent.Link) {
	if !link.Blocking {
		s.deliverBackgroundCompletion(ctx, link)
		return
	}

	won, err := s.subagents.DeliverCompletion(ctx, link, s.completionContent(ctx, link))
	if err != nil {
		logger.Ctx(ctx).
			Named("daemon.completion").
			Error("deliver_completion_dropped", zap.Int64("child", link.ChildID), zap.Int64("parent", link.ParentID), zap.Error(err))

		return
	}

	if won {
		if err := s.rearmChildAfterDelivery(ctx, link.ChildID); err != nil {
			logger.Ctx(ctx).
				Named("daemon.completion").
				Error("rearm_child_after_delivery", zap.Int64("child", link.ChildID), zap.Error(err))
		}
	}
}

func (s *svc) deliverBackgroundCompletion(ctx context.Context, link subagent.Link) {
	child, err := s.store.GetSession(ctx, link.ChildID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.completion").Error(
			"load_background_child", zap.Int64("child", link.ChildID), zap.Error(err),
		)

		return
	}

	won, err := s.subagents.DeliverBackgroundCompletion(ctx, link, child.Iteration)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.completion").Error(
			"deliver_background_completion", zap.Int64("child", link.ChildID), zap.Error(err),
		)

		return
	}

	if !won {
		return
	}

	if err := s.inputReady(ctx, link.ParentID); err != nil {
		logger.Ctx(ctx).Named("daemon.completion").Warn(
			"background_completion_input_ready", zap.Int64("child", link.ChildID), zap.Error(err),
		)
	}
}

// completionContent formats a terminal child's stored result + outcome for the
// parent, via the shared formatter so it matches get_subagent_result verbatim.
func (s *svc) completionContent(ctx context.Context, link subagent.Link) string {
	res := subagent.ChildResult{
		ChildID:  link.ChildID,
		State:    link.State,
		Outcome:  link.Outcome,
		Output:   link.Result,
		Terminal: true,
	}

	if rec, err := s.store.GetSession(ctx, link.ChildID); err == nil {
		res.Iteration = rec.Iteration
	}

	return subagent.FormatChildResult(res)
}
