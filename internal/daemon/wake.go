package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

const (
	statusCommand    = "/status"
	helpCommand      = "/help"
	schedulesCommand = "/schedules"
	compactCommand   = "/compact"
	stopCommand      = "/stop"
	clearCommand     = "/clear"
	killCommand      = "/kill"
)

func isReadOnlyCommand(content string) bool {
	content = strings.TrimSpace(content)

	return content == statusCommand || content == helpCommand || content == schedulesCommand ||
		content == compactCommand || strings.HasPrefix(content, compactCommand+" ")
}

func isEnqueueCommand(content string) bool {
	content = strings.TrimSpace(content)
	return content == statusCommand || content == stopCommand || content == clearCommand || content == killCommand
}

func (s *svc) startWake() {
	s.life.Go("daemon.input", func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.store.Woken():
				for _, id := range s.store.TakeWoken() {
					s.refreshBudget(ctx, id)

					if err := s.inputReady(ctx, id); err != nil {
						logger.Ctx(ctx).
							Named("daemon.input").
							Warn("input_ready_failed", zap.Int64("session_id", id), zap.Error(err))
					}
				}
			case <-s.clock.dueSignal():
				for _, id := range s.clock.takeDue() {
					s.budgetDue(ctx, id)
				}
			}
		}
	})
}

func (s *svc) inputReady(ctx context.Context, id int64) error {
	rec, err := s.store.GetSession(ctx, id)
	if err != nil {
		return fmt.Errorf("load input-ready session %d: %w", id, err)
	}

	rows, err := s.store.ListPending(ctx, id)
	if err != nil {
		return fmt.Errorf("input ready: %w", err)
	}

	if !wakeWorthy(rec, rows) {
		return nil
	}

	runnable, err := s.pendingRunnable(ctx, id, rows)
	if err != nil || !runnable {
		return err
	}

	handled, err := s.handleTerminalChildInputReady(ctx, rec)
	if err != nil || handled {
		return err
	}

	return s.start(ctx, id)
}

func wakeWorthy(rec *sessionstore.SessionRecord, rows []*sessionstore.InboxInput) bool {
	if rec.KilledAt != nil || rec.Status == sessionstore.SessionStatusKilled ||
		rec.Status == sessionstore.SessionStatusStopped || rec.Status == sessionstore.SessionStatusStopping ||
		rec.Status == sessionstore.SessionStatusTerminating {
		return false
	}

	if !slices.ContainsFunc(rows, func(row *sessionstore.InboxInput) bool {
		return row.Source != sessionstore.InputSourceUser || !isEnqueueCommand(row.RawContent)
	}) {
		return false
	}

	if rec.Status == sessionstore.SessionStatusError {
		return rec.ParentID == 0 && slices.ContainsFunc(rows, func(row *sessionstore.InboxInput) bool {
			return row.Source == sessionstore.InputSourceSchedule
		})
	}

	return true
}

func (s *svc) pendingRunnable(ctx context.Context, id int64, rows []*sessionstore.InboxInput) (bool, error) {
	if len(rows) == 0 {
		return false, nil
	}

	for _, row := range rows {
		if row.Source == sessionstore.InputSourceCallResult {
			return true, nil
		}

		if row.Source == sessionstore.InputSourceUser && isReadOnlyCommand(row.RawContent) {
			command := strings.TrimSpace(row.RawContent)

			deferred := s.runners.deferAnnounced(id) &&
				(command == compactCommand || strings.HasPrefix(command, compactCommand+" "))
			if !deferred {
				return true, nil
			}
		}
	}

	calls, err := s.callOwners(ctx, id)
	if err != nil {
		return false, err
	}

	for _, name := range calls {
		if name != tool.IDSleep {
			return false, nil
		}
	}

	return true, nil
}

func (s *svc) commandOnlyStoppedRoot(ctx context.Context, rec *sessionstore.SessionRecord) (bool, error) {
	if rec.Status != sessionstore.SessionStatusStopped || rec.ParentID != 0 {
		return false, nil
	}

	head, err := s.store.PeekPending(ctx, rec.ID)
	if errors.Is(err, sessionstore.ErrNoPendingInput) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("peek stopped root input: %w", err)
	}

	return isReadOnlyCommand(head.RawContent), nil
}

func (s *svc) recoverableRunnable(ctx context.Context, id int64) (bool, error) {
	rec, err := s.store.GetSession(ctx, id)
	if err != nil {
		return false, fmt.Errorf("load recoverable session %d: %w", id, err)
	}

	if rec.Status == sessionstore.SessionStatusStopped || rec.Status == sessionstore.SessionStatusError {
		ids, listErr := s.store.ListSessionsWithRecoverableInput(ctx)
		if listErr != nil {
			return false, fmt.Errorf("classify recoverable session %d: %w", id, listErr)
		}

		if !slices.Contains(ids, id) {
			return false, nil
		}
	}

	_, err = s.store.PeekPending(ctx, id)
	if err == nil {
		rows, listErr := s.store.ListPending(ctx, id)
		if listErr != nil {
			return false, fmt.Errorf("pending input runnable: %w", listErr)
		}

		return s.pendingRunnable(ctx, id, rows)
	}

	if !errors.Is(err, sessionstore.ErrNoPendingInput) {
		return false, fmt.Errorf("peek durable input for session %d: %w", id, err)
	}

	if rec.KilledAt != nil || rec.Status != sessionstore.SessionStatusActive {
		return false, nil
	}

	accepted, err := s.store.HasAcceptedInput(ctx, id)
	if err != nil {
		return false, fmt.Errorf("load accepted input identity for session %d: %w", id, err)
	}

	if !accepted {
		return false, nil
	}

	calls, err := s.callOwners(ctx, id)
	if err != nil {
		return false, err
	}

	return len(calls) == 0, nil
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

	target, err := s.store.GetSession(guarded, child.ID)
	if err != nil {
		return fmt.Errorf("reload input-ready child %d: %w", child.ID, err)
	}

	if target.KilledAt != nil || target.Status == sessionstore.SessionStatusStopped ||
		target.Status == sessionstore.SessionStatusError || target.Status == sessionstore.SessionStatusStopping ||
		target.Status == sessionstore.SessionStatusTerminating {
		return nil
	}

	root, err := s.store.GetSession(guarded, sessionRootID(child))
	if err != nil {
		return fmt.Errorf("load input-ready root for child %d: %w", child.ID, err)
	}

	if root.KilledAt != nil || root.Status == sessionstore.SessionStatusStopped ||
		root.Status == sessionstore.SessionStatusError || root.Status == sessionstore.SessionStatusStopping ||
		root.Status == sessionstore.SessionStatusTerminating {
		return nil
	}

	return s.rearm(guarded, child.ID)
}
