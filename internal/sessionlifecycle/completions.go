package sessionlifecycle

import (
	"context"
	"fmt"
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
	Finalize(ctx context.Context, childID int64, shuttingDown, errored bool) func()
	Persist(ctx context.Context, parent session.Service, link subagent.Link, messages []*transcript.Message) error
	Rearm(ctx context.Context, childID int64) error
	RearmLocked(ctx context.Context, childID int64) error
}

var _ Completions = (*completions)(nil)

type completions struct {
	sessions sessionstore.OrchestrationStore
	links    subagent.Store
	tx       subagent.Transactions

	notifyFailure   func(context.Context, int64, int64, string, error)
	deliver         func(context.Context, subagent.Link)
	startChild      func(context.Context, int64) error
	guardChild      func(context.Context, int64, func(context.Context) error) error
	subagentChanged func(context.Context, int64)
}

func NewCompletions(
	sessions sessionstore.OrchestrationStore,
	links subagent.Store,
	tx subagent.Transactions,
	notifyFailure func(context.Context, int64, int64, string, error),
	deliver func(context.Context, subagent.Link),
	startChild func(context.Context, int64) error,
	guardChild func(context.Context, int64, func(context.Context) error) error,
	subagentChanged func(context.Context, int64),
) Completions {
	return &completions{
		sessions: sessions, links: links, tx: tx,
		notifyFailure: notifyFailure, deliver: deliver, startChild: startChild, guardChild: guardChild,
		subagentChanged: subagentChanged,
	}
}

func (c *completions) Finalize(ctx context.Context, childID int64, shuttingDown, errored bool) func() {
	if shuttingDown {
		return nil
	}

	link, err := c.links.GetLink(ctx, childID)
	if err != nil {
		logger.Ctx(ctx).Named("sessionlifecycle.completion").Error(
			"finalize_get_link", zap.Int64("child", childID), zap.Error(err),
		)

		return nil
	}

	if link == nil || link.Terminal() || link.State == subagent.StateStopped {
		return nil
	}

	record, err := c.sessions.GetSession(ctx, childID)
	if err != nil {
		logger.Ctx(ctx).Named("sessionlifecycle.completion").Error(
			"finalize_get_session", zap.Int64("child", childID), zap.Error(err),
		)
		c.notifyFailure(ctx, link.ParentID, childID, "could not be finalized", err)

		return nil
	}

	if record.Status == sessionstore.SessionStatusSuspended && !errored {
		return nil
	}

	state := subagent.StateCompleted
	persistedStatus := sessionstore.SessionStatusCompleted

	if errored || record.Status == sessionstore.SessionStatusError {
		state = subagent.StateError
		persistedStatus = sessionstore.SessionStatusError
	}

	result, outcome := c.deriveOutcome(
		ctx, childID, record.Iteration, errored, record.Status == sessionstore.SessionStatusError,
	)

	terminalized, err := c.finalizeActivation(ctx, childID, state, result, outcome)
	if err != nil {
		logger.Ctx(ctx).Named("sessionlifecycle.completion").Error(
			"mark_link_terminal", zap.Int64("child", childID), zap.Error(err),
		)
		c.notifyFailure(ctx, link.ParentID, childID, "completion could not be recorded", err)

		return nil
	}

	if !terminalized {
		return nil
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

	return func() { c.deliver(ctx, *link) }
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

	return c.Rearm(ctx, link.ChildID)
}

func (c *completions) Rearm(ctx context.Context, childID int64) error {
	if c.guardChild != nil {
		return c.guardChild(ctx, childID, func(guarded context.Context) error {
			return c.rearm(guarded, childID)
		})
	}

	return c.rearm(ctx, childID)
}

// RearmLocked re-arms a child while the caller holds its session-tree fence.
func (c *completions) RearmLocked(ctx context.Context, childID int64) error {
	return c.rearm(ctx, childID)
}

func (c *completions) rearm(ctx context.Context, childID int64) error {
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
