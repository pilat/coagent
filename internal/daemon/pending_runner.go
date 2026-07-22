package daemon

import (
	"context"
	"errors"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

// enqueuePendingRunner records accepted root work that could not immediately
// acquire an admission slot. Normal input is already durable; this queue is only
// a best-effort ordering cache, rebuilt by the startup recoverable-input selector.
func (s *svc) enqueuePendingRunner(sessionID int64, workDir string, projectID int64) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	for _, pending := range s.pendingRunners {
		if pending.sessionID == sessionID {
			return
		}
	}

	s.pendingRunners = append(s.pendingRunners, queuedRunner{
		sessionID: sessionID,
		workDir:   workDir,
		projectID: projectID,
	})
}

func (s *svc) drainPendingRunners(ctx context.Context) {
	s.pendingMu.Lock()
	if len(s.pendingRunners) == 0 {
		s.pendingMu.Unlock()
		return
	}

	next := s.pendingRunners[0]
	s.pendingRunners = s.pendingRunners[1:]
	s.pendingMu.Unlock()

	err := s.ensureRunner(ctx, next.sessionID, next.workDir, next.projectID, nil)
	if errors.Is(err, errNoCapacity) {
		s.enqueuePendingRunner(next.sessionID, next.workDir, next.projectID)
		return
	}

	if err != nil {
		logger.Ctx(ctx).Named("daemon.admission").Error(
			"pending_runner_start_failed", zap.Int64("session_id", next.sessionID), zap.Error(err))
	}
}
