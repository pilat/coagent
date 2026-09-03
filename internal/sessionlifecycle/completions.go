package sessionlifecycle

import (
	"context"
	"fmt"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/transcript"
)

const (
	completionAttempts = 3
	completionBackoff  = 150 * time.Millisecond
)

type Completions interface {
	Finalize(ctx context.Context, childID int64, shuttingDown, errored bool)
	Persist(ctx context.Context, parent session.Service, link subagent.Link, messages []*transcript.Message) error
	Rearm(ctx context.Context, childID int64) error
}

var _ Completions = (*completions)(nil)

type completions struct {
	sessions sessionstore.OrchestrationStore
	links    subagent.Store
	tx       subagent.Transactions

	notifyFailure   func(context.Context, int64, int64, string, error)
	deliver         func(context.Context, subagent.Link)
	startChild      func(context.Context, int64) error
	subagentChanged func(context.Context, int64)
}

func NewCompletions(
	sessions sessionstore.OrchestrationStore,
	links subagent.Store,
	tx subagent.Transactions,
	notifyFailure func(context.Context, int64, int64, string, error),
	deliver func(context.Context, subagent.Link),
	startChild func(context.Context, int64) error,
	subagentChanged func(context.Context, int64),
) Completions {
	return &completions{
		sessions: sessions, links: links, tx: tx,
		notifyFailure: notifyFailure, deliver: deliver, startChild: startChild,
		subagentChanged: subagentChanged,
	}
}

func (c *completions) Finalize(ctx context.Context, childID int64, shuttingDown, errored bool) {
	if shuttingDown {
		return
	}

	link, err := c.links.GetLink(ctx, childID)
	if err != nil {
		logger.Ctx(ctx).Named("sessionlifecycle.completion").Error(
			"finalize_get_link", zap.Int64("child", childID), zap.Error(err),
		)

		return
	}

	if link == nil || link.Terminal() || link.State == subagent.StateStopped {
		return
	}

	record, err := c.sessions.GetSession(ctx, childID)
	if err != nil {
		logger.Ctx(ctx).Named("sessionlifecycle.completion").Error(
			"finalize_get_session", zap.Int64("child", childID), zap.Error(err),
		)
		c.notifyFailure(ctx, link.ParentID, childID, "could not be finalized", err)

		return
	}

	if record.Status == sessionstore.SessionStatusSuspended && !errored {
		return
	}

	state := subagent.StateCompleted
	persistedStatus := sessionstore.SessionStatusCompleted

	if errored || record.Status == sessionstore.SessionStatusError {
		state = subagent.StateError
		persistedStatus = sessionstore.SessionStatusError
	}

	result, outcome := c.deriveOutcome(ctx, childID, record.Iteration, errored)

	terminalized, err := c.finalizeActivation(ctx, childID, state, result, outcome)
	if err != nil {
		logger.Ctx(ctx).Named("sessionlifecycle.completion").Error(
			"mark_link_terminal", zap.Int64("child", childID), zap.Error(err),
		)
		c.notifyFailure(ctx, link.ParentID, childID, "completion could not be recorded", err)

		return
	}

	if !terminalized {
		return
	}

	if err := c.sessions.UpdateSessionStatus(ctx, childID, persistedStatus); err != nil {
		logger.Ctx(ctx).Named("sessionlifecycle.completion").Warn(
			"update_child_status", zap.Int64("child", childID), zap.Error(err),
		)
	}

	link.State = state
	link.Result = result
	link.Outcome = outcome

	c.subagentChanged(ctx, childID)
	c.deliver(ctx, *link)
}

func (c *completions) Persist(
	ctx context.Context,
	parent session.Service,
	link subagent.Link,
	messages []*transcript.Message,
) error {
	_, won, err := c.tx.DeliverCompletion(
		ctx, link.ParentID, messages, link.ChildID, link.ActivationSeq,
	)
	if err != nil {
		return fmt.Errorf("deliver completion for child %d: %w", link.ChildID, err)
	}

	if won {
		if reloadErr := parent.ReloadDeliveredCompletion(ctx); reloadErr != nil {
			logger.Ctx(ctx).Named("sessionlifecycle.completion").Warn(
				"completion_reload_failed", zap.Int64("child", link.ChildID),
				zap.Int64("parent", link.ParentID), zap.Error(reloadErr),
			)
		}
	}

	return c.Rearm(context.WithoutCancel(ctx), link.ChildID)
}

func (c *completions) Rearm(ctx context.Context, childID int64) error {
	rearmed, err := c.tx.RearmDeliveredWithPendingInput(ctx, childID)
	if err != nil {
		return fmt.Errorf("rearm child %d after completion delivery: %w", childID, err)
	}

	if !rearmed {
		return nil
	}

	if err := c.sessions.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusActive); err != nil {
		return fmt.Errorf("activate rearmed child %d: %w", childID, err)
	}

	c.subagentChanged(ctx, childID)

	if err := c.startChild(ctx, childID); err != nil {
		return fmt.Errorf("start rearmed child %d: %w", childID, err)
	}

	return nil
}

func (c *completions) deriveOutcome(
	ctx context.Context,
	childID int64,
	iterations int,
	errored bool,
) (string, subagent.Outcome) {
	messages, err := c.sessions.LoadActiveMessages(ctx, childID)
	if err != nil {
		messages = nil
	}

	finalText := lastAssistantText(messages)
	switch {
	case errored:
		if finalText == "" {
			finalText = fmt.Sprintf("crashed after %d iterations", iterations)
		}

		return finalText, subagent.OutcomeError
	case lastMessageIsFinalAnswer(messages):
		return finalText, subagent.OutcomeCompleted
	default:
		return fmt.Sprintf("ended without a final answer after %d iterations", iterations),
			subagent.OutcomeIncomplete
	}
}

func (c *completions) finalizeActivation(
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

		finalized, err = c.tx.TryFinalizeActivation(ctx, childID, state, result, outcome)
		if err == nil {
			return finalized, nil
		}
	}

	return false, fmt.Errorf("finalize activation for child %d: %w", childID, err)
}

func lastAssistantText(messages []*transcript.Message) string {
	for _, message := range slices.Backward(messages) {
		if message.Role == "assistant" && len(message.ToolCalls) == 0 && message.Content != "" {
			return message.Content
		}
	}

	return ""
}

func lastMessageIsFinalAnswer(messages []*transcript.Message) bool {
	for _, message := range slices.Backward(messages) {
		switch message.Role {
		case "assistant":
			return len(message.ToolCalls) == 0 && message.Content != ""
		case "user":
			return false
		}
	}

	return false
}
