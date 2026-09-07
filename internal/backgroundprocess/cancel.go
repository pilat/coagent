package backgroundprocess

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CancelProcess cancels and joins one exact durable process.
func (s *svc) CancelProcess(ctx context.Context, processID string, intent HostIntent) (int, error) {
	process, err := s.store.GetProcess(ctx, processID)
	if err != nil {
		cancel := s.trackedCancelWithFallback(processID, intent)
		if cancel != nil {
			cancel()
		}

		return 0, errors.Join(
			fmt.Errorf("load process for cancellation: %w", err),
			s.waitForUntracked(ctx, processID),
		)
	}

	if process.State.Terminal() {
		return 0, nil
	}

	return s.cancelRecords(ctx, []Process{process}, intent)
}

func (s *svc) waitForUntracked(ctx context.Context, processID string) error {
	for s.trackedCancel(processID) != nil {
		select {
		case <-ctx.Done():
			return fmt.Errorf("join process %s: %w", processID, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}

	return nil
}

func (s *svc) CancelTree(
	ctx context.Context,
	rootSessionID int64,
	intent HostIntent,
) (int, error) {
	running, err := s.store.ListRunningByRoot(ctx, rootSessionID)
	if err != nil {
		cancelled, joinErr := s.cancelTrackedMatching(ctx, intent, func(process Process) bool {
			return process.RootSessionID == rootSessionID
		})

		return cancelled, errors.Join(fmt.Errorf("list tree processes: %w", err), joinErr)
	}

	return s.cancelRecords(ctx, running, intent)
}

func (s *svc) CancelSessions(
	ctx context.Context,
	sessionIDs []int64,
	intent HostIntent,
) (int, error) {
	running, err := s.store.ListRunningBySessions(ctx, sessionIDs)
	if err != nil {
		owners := make(map[int64]struct{}, len(sessionIDs))
		for _, sessionID := range sessionIDs {
			owners[sessionID] = struct{}{}
		}

		cancelled, joinErr := s.cancelTrackedMatching(ctx, intent, func(process Process) bool {
			_, matches := owners[process.SessionID]

			return matches
		})

		return cancelled, errors.Join(fmt.Errorf("list session processes: %w", err), joinErr)
	}

	return s.cancelRecords(ctx, running, intent)
}

func (s *svc) CancelAll(ctx context.Context, intent HostIntent) (int, error) {
	s.closeAdmission()

	running, err := s.store.ListRunning(ctx)
	if err != nil {
		cancelled, joinErr := s.cancelTrackedMatching(ctx, intent, func(Process) bool { return true })

		return cancelled, errors.Join(
			fmt.Errorf("list running processes: %w", err),
			joinErr,
			s.waitForNoLive(ctx),
		)
	}

	cancelled, cancelErr := s.cancelRecords(ctx, running, intent)
	joinErr := s.waitForNoLive(ctx)

	return cancelled, errors.Join(cancelErr, joinErr)
}

func (s *svc) InterruptNonterminal(ctx context.Context) (int, error) {
	running, err := s.store.ListRunningAdvertised(ctx)
	if err != nil {
		return 0, fmt.Errorf("list leftover processes: %w", err)
	}

	interrupted := 0

	for _, process := range running {
		_, ok, err := s.store.Finalize(ctx, process.ID, StateInterrupted, nil, process.OutputSize)
		if err != nil {
			return interrupted, fmt.Errorf("interrupt process %s: %w", process.ID, err)
		}

		if !ok {
			continue
		}

		interrupted++

		s.mu.Lock()
		cancel := s.cancels[process.ID]
		s.mu.Unlock()

		if cancel != nil {
			cancel()
		}
	}

	return interrupted, nil
}

func (s *svc) cancelRecords(
	ctx context.Context,
	running []Process,
	intent HostIntent,
) (int, error) {
	cancelled := 0
	var cancelErr error

	for _, process := range running {
		won, err := s.cancelRecord(ctx, process, intent)
		if won {
			cancelled++
		}

		cancelErr = errors.Join(cancelErr, err)
	}

	return cancelled, errors.Join(cancelErr, s.joinCancelled(ctx, running))
}

func (s *svc) cancelRecord(ctx context.Context, process Process, intent HostIntent) (bool, error) {
	won, recordErr := s.store.RecordIntent(ctx, process.ID, intent)
	if recordErr != nil {
		recordErr = fmt.Errorf("record cancel intent for %s: %w", process.ID, recordErr)
	}

	var cancel context.CancelFunc
	if recordErr != nil {
		cancel = s.trackedCancelWithFallback(process.ID, intent)
	} else {
		cancel = s.trackedCancel(process.ID)
	}

	if cancel != nil {
		cancel()

		return won, recordErr
	}

	terminalIntent := process.HostIntent
	if won || recordErr != nil {
		terminalIntent = intent
	}

	if terminalIntent == IntentNone {
		return won, recordErr
	}

	var finalizeErr error

	if recordErr != nil {
		_, _, finalizeErr = s.store.FinalizeWithIntent(ctx, process.ID, intent, process.OutputSize)
	} else {
		_, _, finalizeErr = s.store.Finalize(
			ctx, process.ID, IntentToState(terminalIntent), nil, process.OutputSize,
		)
	}

	if finalizeErr != nil {
		finalizeErr = fmt.Errorf("terminalize detached process %s: %w", process.ID, finalizeErr)
	}

	return won, errors.Join(recordErr, finalizeErr)
}

func (s *svc) joinCancelled(ctx context.Context, running []Process) error {
	if len(running) == 0 {
		return nil
	}

	pending := make(map[string]struct{}, len(running))
	var joinErr error

	for _, process := range running {
		pending[process.ID] = struct{}{}
	}

	deadline := time.Now().Add(joinGrace)
	for time.Now().Before(deadline) {
		for id := range pending {
			process, err := s.store.GetProcess(ctx, id)
			if err != nil {
				joinErr = errors.Join(joinErr,
					fmt.Errorf("join cancelled process %s: %w", id, err),
				)
				delete(pending, id)

				continue
			}

			if process.State.Terminal() {
				delete(pending, id)
			}
		}

		if len(pending) == 0 {
			return joinErr
		}

		select {
		case <-ctx.Done():
			return errors.Join(joinErr, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}

	return errors.Join(joinErr,
		fmt.Errorf("join %d cancelled processes: %w", len(pending), context.DeadlineExceeded),
	)
}
