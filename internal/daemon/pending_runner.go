package daemon

import (
	"context"
	"errors"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/logger"
)

// enqueuePendingRunner records accepted root work that could not immediately
// acquire an admission slot. Normal input is already durable; this queue is only
// a best-effort ordering cache, rebuilt by the startup recoverable-input selector.
func (s *svc) enqueuePendingRunner(sessionID int64, workDir string, projectID int64) {
	s.pendingQueue.PushUnique(queuedRunner{
		sessionID: sessionID,
		workDir:   workDir,
		projectID: projectID,
	}, func(left, right queuedRunner) bool {
		return left.sessionID == right.sessionID
	})
}

func (s *svc) drainPendingRunners(ctx context.Context) {
	next, ok := s.pendingQueue.PopFirst(func(queuedRunner) bool { return true })
	if !ok {
		return
	}

	err := s.ensureRunner(ctx, next.sessionID, next.workDir, next.projectID, nil)
	if errors.Is(err, admission.ErrNoCapacity) {
		s.enqueuePendingRunner(next.sessionID, next.workDir, next.projectID)
		return
	}

	if err != nil {
		logger.Ctx(ctx).Named("daemon.admission").Error(
			"pending_runner_start_failed", zap.Int64("session_id", next.sessionID), zap.Error(err))
	}
}
