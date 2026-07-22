package daemon

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
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
	s.notifyChildFailure(parentID, childID, "could not resolve its wall-clock timeout", err)
}

// notifyChildFailure reports on the PARENT's topic: a child that never reached
// announceSession has no topic of its own, so publishing to it reaches nobody.
func (s *svc) notifyChildFailure(parentID, childID int64, what string, err error) {
	if parentID == 0 {
		return
	}

	s.publish(parentID, sessionevent.Notification{
		Type:    sessionevent.NotifyMessage,
		Message: fmt.Sprintf("⚠️ Subagent %d: %s — %s", childID, what, err),
	})
}
