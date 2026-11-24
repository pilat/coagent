package daemon

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (h orchestratorHost) Subscribe(ctx context.Context, callerID, targetID int64) (int64, bool, error) {
	_, err := h.caller(ctx, callerID)
	if err != nil {
		return 0, false, err
	}

	target, err := h.sendTarget(ctx, targetID)
	if err != nil {
		return 0, false, err
	}

	if target.ID == callerID {
		return 0, false, errors.New("cannot subscribe to this session itself")
	}

	states, err := h.s.store.WatchStates(ctx, target.ID)
	if err != nil {
		return 0, false, fmt.Errorf("load target watches: %w", err)
	}

	if states[callerID].Subscribed {
		return 0, false, fmt.Errorf(
			"session #%d is already subscribed to this session; two sessions cannot subscribe to each other",
			target.ID,
		)
	}

	unread, err := h.s.store.ArmWatch(ctx, callerID, target.ID, true)
	if err != nil {
		return 0, false, fmt.Errorf("subscribe session watch: %w", err)
	}

	return target.ID, unread, nil
}

func (h orchestratorHost) Unsubscribe(ctx context.Context, callerID, targetID int64) (int64, bool, error) {
	_, err := h.caller(ctx, callerID)
	if err != nil {
		return 0, false, err
	}

	if err := h.rootTarget(ctx, targetID); err != nil {
		return 0, false, err
	}

	chain, err := h.s.store.ReplacementChain(ctx, targetID)
	if err != nil {
		return 0, false, fmt.Errorf("resolve unsubscribe target: %w", err)
	}

	newest := chain[len(chain)-1]

	was, err := h.s.store.Unsubscribe(ctx, callerID, newest)
	if err != nil {
		return newest, was, fmt.Errorf("unsubscribe session watch: %w", err)
	}

	return newest, was, nil
}

func (s *Service) signalOutcome(ctx context.Context, targetID int64, kind orchestrator.SignalKind, oldID, actor int64) {
	if ctx.Err() != nil {
		return
	}

	if err := s.fireOutcome(ctx, targetID, kind, oldID, actor); err != nil {
		logger.Ctx(ctx).Named("daemon.orchestrator").Error("signal_outcome_failed",
			zap.Int64("session_id", targetID), zap.Error(err))
	}
}

func (s *Service) fireOutcome(
	ctx context.Context,
	targetID int64,
	kind orchestrator.SignalKind,
	oldID, actor int64,
) error {
	recordID := targetID
	if oldID != 0 {
		recordID = oldID
	}

	record, err := s.store.GetSession(ctx, recordID)
	if err != nil {
		return fmt.Errorf("load outcome session: %w", err)
	}

	if record.ParentID != 0 {
		return nil
	}

	// Empty-response termination persists an error without returning a run error.
	if record.Status == sessionstore.SessionStatusError && (kind == "" || kind == orchestrator.SignalAnswered) {
		kind = orchestrator.SignalError
	}

	if kind == "" {
		return nil
	}

	if kind == orchestrator.SignalAnswered {
		quiet, err := s.outcomeQuiet(ctx, targetID)
		if err != nil || !quiet {
			return err
		}
	}

	project, err := s.store.GetProjectName(ctx, record.ProjectID)
	if err != nil {
		return fmt.Errorf("load outcome project: %w", err)
	}

	title, _ := record.Attributes[controllerapi.SessionAttributeTitle].(string)
	name := sessionstore.ChatDisplayName(project, title)
	line, notice := orchestrator.SignalText(kind, recordID, project, name, targetID, actor)

	_, err = s.store.FireWatches(ctx, sessionstore.WatchFire{
		TargetID: targetID, ActorID: actor, Line: line, Notice: notice,
	})
	if err != nil {
		return fmt.Errorf("fire outcome watches: %w", err)
	}

	return nil
}

func (s *Service) outcomeQuiet(ctx context.Context, targetID int64) (bool, error) {
	_, err := s.store.PeekPending(ctx, targetID)
	switch {
	case errors.Is(err, sessionstore.ErrNoPendingInput):
	case err != nil:
		return false, fmt.Errorf("peek outcome input: %w", err)
	default:
		return false, nil
	}

	wake, err := s.store.HasBackgroundWakeSource(ctx, targetID)
	if err != nil {
		return false, fmt.Errorf("check outcome wake source: %w", err)
	}

	return !wake, nil
}

func runSignalKind(out runOutcome, errored bool) orchestrator.SignalKind {
	if errored {
		return orchestrator.SignalError
	}

	if out.answered && !out.suspended && !out.budgetFired {
		return orchestrator.SignalAnswered
	}

	return ""
}
