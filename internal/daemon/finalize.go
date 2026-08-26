package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	// A child killed with a non-terminal link is invisible to the sweep forever,
	// so the terminal mark is worth retrying rather than logging once.
	linkTerminalAttempts = 3
	linkTerminalBackoff  = 150 * time.Millisecond

	// cascadeRetryBudget caps retries across a WHOLE cascade kill, not per node:
	// the walk is sequential and sits on the synchronous Kill path, so a per-node
	// budget multiplies by the size of the tree.
	cascadeRetryBudget = 2 * time.Second
)

// markLinkTerminalRetrying retries MarkLinkTerminal until it succeeds, runs out of
// attempts, or passes deadline (zero deadline = attempts only). The pause is
// uninterruptible: callers pass a WithoutCancel ctx, so Done() never fires.
func (s *svc) markLinkTerminalRetrying(
	ctx context.Context,
	deadline time.Time,
	childID int64,
	state LinkState,
	result string,
	outcome LinkOutcome,
) error {
	var err error

	for attempt := range linkTerminalAttempts {
		if attempt > 0 && !deadline.IsZero() && time.Now().After(deadline) {
			return err
		}

		if attempt > 0 {
			time.Sleep(linkTerminalBackoff)
		}

		err = s.links.MarkLinkTerminal(ctx, childID, state, result, outcome)
		if err == nil {
			return nil
		}
	}

	return fmt.Errorf("mark link terminal for child %d: %w", childID, err)
}

func (s *svc) finalizeActivationRetrying(
	ctx context.Context,
	childID int64,
	state LinkState,
	result string,
	outcome LinkOutcome,
) (bool, error) {
	var err error

	for attempt := range linkTerminalAttempts {
		if attempt > 0 {
			time.Sleep(linkTerminalBackoff)
		}

		var finalized bool

		finalized, err = s.sessionStore.TryFinalizeSubagentActivation(
			ctx, childID, string(state), result, string(outcome),
		)
		if err == nil {
			return finalized, nil
		}
	}

	return false, fmt.Errorf("finalize activation for child %d: %w", childID, err)
}

// reportTimeoutUnresolved stays silent when ctx is already dead: shutdown, kill
// and stop cancel the loop ctx, and that is what failed the read, not the ledger.
func (s *svc) reportTimeoutUnresolved(ctx context.Context, parentID, childID int64, err error) {
	if ctx.Err() != nil {
		return
	}

	logger.Ctx(ctx).Named("daemon.runner").
		Error("child_timeout_unresolved", zap.Int64("session_id", childID), zap.Error(err))
	s.notifyChildFailure(ctx, parentID, childID, "could not resolve its wall-clock timeout", err)
}

// notifyChildFailure reports on the PARENT's topic: a child that never reached
// announceSession has no topic of its own, so publishing to it reaches nobody.
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
	outputs := s.OutputStore()
	if outputs == nil {
		return nil
	}

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
