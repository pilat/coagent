package daemon

import "github.com/pilat/coagent/internal/sessionlifecycle"

func (s *svc) newCompletionCoordinator() sessionlifecycle.Completions {
	return sessionlifecycle.NewCompletions(
		s.sessionStore, s.links, s.subagents,
		s.notifyChildFailure, s.deliverCompletionToParent, s.ensureSessionRunner,
	)
}
