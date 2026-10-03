package daemon

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

var _ subagent.Spawner = (*svc)(nil)

// Spawn starts a durably linked child and returns its ID without waiting.
func (s *svc) Spawn(ctx context.Context, req subagent.SpawnRequest) (subagent.ChildResult, error) {
	unlock, err := s.lockSessionTree(ctx, req.ParentID)
	if err != nil {
		return subagent.ChildResult{}, err
	}
	defer unlock()

	childID, _, _, err := s.createChildSession(ctx, req)
	if err != nil {
		return subagent.ChildResult{}, fmt.Errorf("guard child spawn: %w", err)
	}

	if err := s.startLocked(context.WithoutCancel(ctx), childID); err != nil {
		return subagent.ChildResult{}, fmt.Errorf("guard child spawn: start child runner: %w", err)
	}

	return subagent.ChildResult{ChildID: childID, State: subagent.StateSpawned}, nil
}

// Result returns a snapshot of a child's state and (once terminal) its output.
func (s *svc) Result(ctx context.Context, childID int64) (subagent.ChildResult, error) {
	return s.childSnapshot(ctx, childID)
}

// SendToChild preserves completion delivery before rearming a terminal child.
func (s *svc) SendToChild(ctx context.Context, childID int64, msg string) error {
	requestCtx := ctx

	unlock, err := s.lockSessionTree(ctx, childID)
	if err != nil {
		return err
	}

	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()

	ctx = context.WithoutCancel(ctx)

	link, err := s.enqueueChildFollowUpLocked(ctx, childID, msg)
	if err != nil {
		return err
	}

	if link.State == subagent.StateStopped {
		return s.resumeStoppedChildLocked(ctx, childID)
	}

	if link.State == subagent.StateError && link.DeliveredAt != 0 {
		return s.resumeChildWithPendingInputLocked(ctx, childID)
	}

	unlock()

	locked = false

	if link.Terminal() {
		return s.finishChildFollowUp(requestCtx, *link)
	}

	return s.start(requestCtx, childID)
}

func (s *svc) enqueueChildFollowUpLocked(ctx context.Context, childID int64, msg string) (*subagent.Link, error) {
	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		return nil, fmt.Errorf("load subagent link: %w", err)
	}

	if link == nil {
		return nil, fmt.Errorf("subagent %d not found", childID)
	}

	if link.State == subagent.StateKilled {
		return nil, fmt.Errorf("subagent %d is killed", childID)
	}

	if _, err := s.store.Enqueue(
		ctx,
		sessionstore.Input{SessionID: childID, Source: sessionstore.InputSourceAgent, Content: msg},
	); err != nil {
		return nil, fmt.Errorf("persist subagent follow-up: %w", err)
	}

	// Re-read after enqueue. The child may have crossed its terminal boundary
	// while the durable write committed.
	link, err = s.links.GetLink(ctx, childID)
	if err != nil {
		return nil, fmt.Errorf("reload subagent link: %w", err)
	}

	if link == nil {
		return nil, fmt.Errorf("subagent %d disappeared after accepting follow-up", childID)
	}

	return link, nil
}

func (s *svc) resumeStoppedChildLocked(ctx context.Context, childID int64) error {
	rec, recErr := s.store.GetSession(ctx, childID)
	if recErr != nil {
		return fmt.Errorf("load stopped subagent session: %w", recErr)
	}

	if rec.Status == sessionstore.SessionStatusStopping {
		return nil // /stop won; it will cancel this accepted input
	}

	return s.resumeChildWithPendingInputLocked(ctx, childID)
}

func (s *svc) finishChildFollowUp(ctx context.Context, link subagent.Link) error {
	childID := link.ChildID
	if link.DeliveredAt != 0 {
		return s.rearmChildAfterDelivery(ctx, childID)
	}

	s.deliverCompletionToParent(ctx, link)

	updated, err := s.links.GetLink(ctx, childID)
	if err != nil {
		return fmt.Errorf("reload delivered subagent link: %w", err)
	}

	if updated == nil || updated.DeliveredAt == 0 {
		return nil
	}

	if updated.State != subagent.StateError {
		return s.rearmChildAfterDelivery(ctx, childID)
	}

	unlock, err := s.lockSessionTree(ctx, childID)
	if err != nil {
		return err
	}
	defer unlock()

	return s.resumeChildWithPendingInputLocked(context.WithoutCancel(ctx), childID)
}

// LinkPending prevents unresolved task calls from spawning the same child twice.
func (s *svc) LinkPending(ctx context.Context, parentID int64, taskCallID string) (bool, error) {
	link, err := s.links.GetLinkByTaskCallID(ctx, parentID, taskCallID)
	if err != nil {
		return false, fmt.Errorf("check pending link: %w", err)
	}

	return link != nil, nil
}

func (s *svc) createChildSession(ctx context.Context, req subagent.SpawnRequest) (int64, string, int64, error) {
	parentRec, err := s.store.GetSession(ctx, req.ParentID)
	if err != nil {
		return 0, "", 0, fmt.Errorf("parent session %d: %w", req.ParentID, err)
	}

	depth, err := s.childDepth(ctx, req.ParentID)
	if err != nil {
		return 0, "", 0, err
	}

	if depth >= maxDepth {
		return 0, "", 0, fmt.Errorf(
			"subagent nesting limit reached (depth %d): do this work inline instead of delegating further",
			depth,
		)
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, parentRec.ProjectID)
	if err != nil {
		return 0, "", 0, fmt.Errorf("resolve parent workdir: %w", err)
	}

	rootID := parentRec.RootID
	if rootID == 0 {
		rootID = req.ParentID
	}

	model := s.resolveChildModel(req, parentRec)

	if err := s.checkBudgetModel(ctx, rootID, model); errors.Is(err, errUnpricedModel) {
		return 0, "", 0, errors.New("cannot spawn an armed budget tree onto a model without catalog pricing")
	} else if err != nil {
		return 0, "", 0, fmt.Errorf("load root budget for child model: %w", err)
	}

	reasoning, err := s.models.effort(model, req.ReasoningLevel, parentRec.ReasoningLevel)
	if err != nil {
		return 0, "", 0, err
	}

	childID, err := s.links.Create(ctx, subagent.Create{
		ProjectID:      parentRec.ProjectID,
		ParentID:       req.ParentID,
		RootID:         rootID,
		AgentType:      req.AgentType,
		Model:          model,
		ReasoningLevel: reasoning,
		TaskCallID:     req.TaskCallID,
		Blocking:       req.Blocking,
		Depth:          depth,
		State:          subagent.StateSpawned,
		InitialInput:   req.Prompt,
	})
	if err != nil {
		return 0, "", 0, fmt.Errorf("create subagent with link: %w", err)
	}

	s.publishSubagentProgress(ctx, childID)

	return childID, workDir, parentRec.ProjectID, nil
}

func (s *svc) resumeChildWithPendingInputLocked(ctx context.Context, childID int64) error {
	if err := s.links.Resume(ctx, childID); err != nil {
		return fmt.Errorf("resume subagent link: %w", err)
	}

	s.publishSubagentProgress(ctx, childID)

	return s.startLocked(ctx, childID)
}

func (s *svc) childSnapshot(ctx context.Context, childID int64) (subagent.ChildResult, error) {
	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		return subagent.ChildResult{}, fmt.Errorf("get link: %w", err)
	}

	if link == nil {
		return subagent.ChildResult{}, fmt.Errorf("subagent %d not found", childID)
	}

	res := subagent.ChildResult{
		ChildID:  childID,
		State:    link.State,
		Terminal: link.Terminal(),
	}

	if rec, rerr := s.store.GetSession(ctx, childID); rerr == nil {
		res.Iteration = rec.Iteration
	}

	// Rearming clears delivery but retains the previous result until terminalization.
	if res.Terminal {
		res.Output = link.Result
		res.Outcome = link.Outcome
	}

	return res, nil
}

func (s *svc) childDepth(ctx context.Context, parentID int64) (int, error) {
	link, err := s.links.GetLink(ctx, parentID)
	if err != nil {
		return 0, fmt.Errorf("parent link %d: %w", parentID, err)
	}

	if link == nil {
		return 1, nil
	}

	return link.Depth + 1, nil
}

func (s *svc) resolveChildModel(req subagent.SpawnRequest, parentRec *sessionstore.SessionRecord) string {
	switch {
	case req.Model != "":
		return req.Model
	case req.AgentModel != "":
		return req.AgentModel
	default:
		return parentRec.Model
	}
}

func (s *svc) finalizeChildLocked(ctx context.Context, childID int64, errored bool) func() {
	link, err := s.links.Finalize(ctx, childID, errored)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.completion").Error("finalize_child", zap.Int64("child", childID), zap.Error(err))

		if link != nil {
			s.notifyChildFailure(ctx, link.ParentID, childID, "completion could not be recorded", err)
		}

		return nil
	}

	if link == nil {
		return nil
	}

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
	rearmed, err := s.links.Rearm(ctx, childID)
	if err != nil {
		return fmt.Errorf("rearm child %d after completion delivery: %w", childID, err)
	}

	if !rearmed {
		return nil
	}

	s.publishSubagentProgress(ctx, childID)

	if err := s.startLocked(ctx, childID); err != nil {
		return fmt.Errorf("start rearmed child %d: %w", childID, err)
	}

	return nil
}

func (s *svc) deliverCompletionToParent(ctx context.Context, link subagent.Link) {
	if !link.Blocking {
		s.deliverBackgroundCompletion(ctx, link)
		return
	}

	won, err := s.links.DeliverCompletion(ctx, link, s.completionContent(ctx, link))
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

	won, err := s.links.DeliverBackgroundCompletion(ctx, link, child.Iteration)
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

// A child that failed before announcement has no topic, so failures go to its parent.
func (s *svc) notifyChildFailure(ctx context.Context, parentID, childID int64, what string, err error) {
	if parentID == 0 {
		return
	}

	message := fmt.Sprintf("⚠️ Subagent %d: %s — %s", childID, what, logger.Redact(err.Error()))
	if outputErr := s.enqueueChildFailureOutput(ctx, parentID, childID, message); outputErr != nil {
		logger.Named("daemon.finalize").Warn("enqueue_child_failure_output", zap.Error(outputErr))
	}

	s.publish(parentID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: message})
}

func (s *svc) enqueueChildFailureOutput(ctx context.Context, parentID, childID int64, message string) error {
	outputs := s.store

	link, err := s.links.GetLink(ctx, childID)
	if err != nil || link == nil || link.ParentID != parentID || link.ActivationSeq <= 0 {
		return s.enqueuePersistentOutput(ctx, parentID, message)
	}

	attributes := map[string]any{"source": "agent"}

	_, err = outputs.EnqueueOutput(ctx, sessionstore.OutputDraft{
		SessionID:  parentID,
		Type:       sessionstore.OutputMessagePersistent,
		Content:    message,
		Attributes: attributes,
		SourceKey:  fmt.Sprintf("child:%d:%d:outcome", childID, link.ActivationSeq),
		Fingerprint: sessionstore.OutputFingerprint(
			sessionstore.OutputMessagePersistent,
			message,
			parentID,
			attributes,
		),
	})
	if errors.Is(err, sessionstore.ErrOutputOwner) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("enqueue child failure output: %w", err)
	}

	return nil
}

func (s *svc) killDescendants(ctx context.Context, parentID int64, depth int) {
	if depth >= maxDepth {
		return
	}

	// Pending links include terminal-undelivered and queued children; the
	// terminal guard preserves the former's stored outcome.
	links, err := s.links.ListPendingChildLinks(ctx, parentID)
	if err != nil {
		// The walk stops here, so part of the subtree survives the teardown.
		logger.Ctx(ctx).Named("daemon.completion").
			Error("cascade_list_children", zap.Int64("parent", parentID), zap.Error(err))

		return
	}

	for _, link := range links {
		if link.Terminal() {
			if !link.Blocking {
				s.deliverBackgroundCompletion(ctx, link)
			}

			continue // already done (e.g. completed-but-undelivered) — keep its result
		}

		s.killDescendants(ctx, link.ChildID, depth+1)
		s.warnKilledDescendant(ctx, link)
		s.killSubagent(ctx, link.ChildID)
	}
}

func (s *svc) warnKilledDescendant(ctx context.Context, link subagent.Link) {
	iteration := 0
	if rec, err := s.store.GetSession(ctx, link.ChildID); err == nil {
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

// The terminal link precedes runner cancellation so teardown cannot deliver a killed activation.
func (s *svc) killSubagent(ctx context.Context, childID int64) {
	if err := s.retireTreeToolResources(ctx, childID); err != nil {
		logger.Ctx(ctx).Named("daemon.completion").Warn("retire_child_tools", zap.Error(err))
	}

	if err := s.links.Kill(ctx, childID); err != nil {
		logger.Ctx(ctx).
			Named("daemon.completion").
			Error("kill_link_terminal", zap.Int64("child", childID), zap.Error(err))
	}

	s.removeSchedules(ctx, childID)

	if rs, ok := s.runners.load(childID); ok {
		rs.Stop()
	}
}
