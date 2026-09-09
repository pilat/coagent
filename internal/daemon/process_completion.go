package daemon

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// routeProcessCompletion is deliberately only a wake hint. The process store
// committed the terminal fact and its inbox row together before calling this.
func (s *svc) routeProcessCompletion(ctx context.Context, completion backgroundprocess.Completion) {
	if err := s.inputReady(ctx, completion.SessionID); err != nil {
		logger.Ctx(ctx).Named("daemon.process").Warn(
			"process_input_ready_failed", zap.String("process", completion.ProcessID), zap.Error(err),
		)
	}
}

// inputReady starts an eligible runner after a durable asynchronous input was
// committed. Stopped and errored sessions retain facts until explicit resume.
func (s *svc) inputReady(ctx context.Context, sessionID int64) error {
	record, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load input-ready session %d: %w", sessionID, err)
	}

	if record.KilledAt != nil || record.Status == sessionstore.SessionStatusStopped ||
		record.Status == sessionstore.SessionStatusError || record.Status == sessionstore.SessionStatusStopping ||
		record.Status == sessionstore.SessionStatusTerminating {
		return nil
	}

	handled, err := s.handleTerminalChildInputReady(ctx, record)
	if err != nil {
		return err
	}

	if handled {
		return nil
	}

	return s.routeQueuedSessionInput(ctx, sessionID, asyncSessionInput{value: inboxReadyInput{}})
}

func (s *svc) handleTerminalChildInputReady(
	ctx context.Context,
	record *sessionstore.SessionRecord,
) (bool, error) {
	if record.ParentID == 0 {
		return false, nil
	}

	link, err := s.links.GetLink(ctx, record.ID)
	if err != nil {
		return false, fmt.Errorf("load input-ready subagent link %d: %w", record.ID, err)
	}

	if link == nil || !link.Terminal() {
		return false, nil
	}

	if link.State == subagent.StateError {
		return true, nil
	}

	if link.DeliveredAt == 0 {
		s.deliverCompletionToParent(ctx, *link)
	}

	return true, s.rearmChildForAsyncInput(ctx, record)
}

func (s *svc) rearmChildForAsyncInput(ctx context.Context, child *sessionstore.SessionRecord) error {
	unlock, err := s.lockSessionTree(ctx, child.ID)
	if err != nil {
		return err
	}
	defer unlock()

	guarded := context.WithoutCancel(ctx)

	target, err := s.sessionStore.GetSession(guarded, child.ID)
	if err != nil {
		return fmt.Errorf("reload input-ready child %d: %w", child.ID, err)
	}

	if target.KilledAt != nil || target.Status == sessionstore.SessionStatusStopped ||
		target.Status == sessionstore.SessionStatusError || target.Status == sessionstore.SessionStatusStopping ||
		target.Status == sessionstore.SessionStatusTerminating {
		return nil
	}

	root, err := s.sessionStore.GetSession(guarded, sessionRootID(child))
	if err != nil {
		return fmt.Errorf("load input-ready root for child %d: %w", child.ID, err)
	}

	if root.KilledAt != nil || root.Status == sessionstore.SessionStatusStopped ||
		root.Status == sessionstore.SessionStatusError || root.Status == sessionstore.SessionStatusStopping ||
		root.Status == sessionstore.SessionStatusTerminating {
		return nil
	}

	return s.completions.RearmLocked(guarded, child.ID) //nolint:wrapcheck // Component owns rearm context.
}

func (s *svc) newDaemonWorkerContext(ctx context.Context) (context.Context, context.CancelFunc) {
	workerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(s.workerCtx, cancel)

	return workerCtx, func() {
		stop()
		cancel()
	}
}
