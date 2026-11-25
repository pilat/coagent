package session

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

const stopHookContinuationCap = 8

// StopHook returns a continuation reason, or an error only when ctx ends.
type StopHook interface {
	Stop(ctx context.Context, lastMessage string, active bool) (reason string, err error)
}

func (s *Session) publishStop(
	ctx context.Context,
	r *runState,
	c *sessionstore.Commit,
	text string,
	state *sessionstore.CompletionCheckState,
) (bool, error) {
	continued, err := s.continueFromStopHook(ctx, r, c, text, state)
	if err != nil {
		return false, err
	}

	if continued {
		return false, nil
	}

	s.finalParts(c, text)
	r.result.Final = text
	r.terminal = true

	return true, nil
}

func (s *Session) continueFromStopHook(
	ctx context.Context,
	r *runState,
	c *sessionstore.Commit,
	text string,
	state *sessionstore.CompletionCheckState,
) (bool, error) {
	if s.stopHook == nil || s.parentID != 0 {
		return false, nil
	}

	if r.stopHookContinuations >= stopHookContinuationCap {
		logger.Ctx(ctx).Info("stop_hook_cap_reached")
		return false, nil
	}

	reason, err := s.stopHook.Stop(ctx, text, r.stopHookContinuations > 0)
	if err != nil {
		return false, fmt.Errorf("run stop hook: %w", err)
	}

	if reason == "" {
		return false, nil
	}

	r.stopHookContinuations++
	c.State.Candidate = &sessionstore.CandidateChange{
		Expected: durableCandidateID(state),
		NextRef:  -1,
	}
	c.Unfired.State.MarkCompletionNudge = true
	c.Unfired.Messages = []*transcript.Message{hostUserMessage(sessionprompt.RenderStopHookFeedback(reason))}

	return true, nil
}
