package daemon

import "context"

func (s *svc) enqueuePendingRunner(sessionID int64, workDir string, projectID int64) {
	s.supervisor.QueueRoot(sessionID, workDir, projectID)
}

func (s *svc) drainPendingRunners(ctx context.Context) { s.supervisor.DrainRoots(ctx) }
