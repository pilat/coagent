package daemon

import (
	"context"

	"github.com/pilat/coagent/internal/sessionlifecycle"
)

func (s *svc) newCompletionCoordinator() sessionlifecycle.Completions {
	return sessionlifecycle.NewCompletions(
		s.sessionStore, s.links, s.subagents,
		s.notifyChildFailure, s.deliverCompletionToParent, s.ensureSessionRunnerLocked,
		s.guardChildTransition,
		s.publishSubagentProgress,
	)
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
