package sessionlifecycle

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func (c *completions) recoveredOutcome(
	ctx context.Context,
	childID int64,
	errored bool,
) (*sessionstore.ActivationOutcome, subagent.Outcome, error) {
	outcome, err := c.sessions.LoadActivationOutcome(ctx, childID, errored)
	if err != nil {
		return nil, "", fmt.Errorf("load activation outcome: %w", err)
	}

	if outcome.Diagnostic != nil {
		logger.Ctx(ctx).Named("sessionlifecycle.completion").Error("load_activation_outcome",
			zap.Int64("child", childID), zap.Error(outcome.Diagnostic))
	}

	switch outcome.Kind {
	case sessionstore.ActivationCompleted:
		return outcome, subagent.OutcomeCompleted, nil
	case sessionstore.ActivationFailed:
		return outcome, subagent.OutcomeError, nil
	case sessionstore.ActivationIncomplete:
		return outcome, subagent.OutcomeIncomplete, nil
	default:
		return nil, "", fmt.Errorf("unknown activation outcome %q", outcome.Kind)
	}
}
