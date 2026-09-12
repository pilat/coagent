package backgroundprocess

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type admissionClass uint8

const (
	admissionCandidate admissionClass = iota + 1
	admissionBackground
)

//nolint:wsl_v5 // Candidate/background accounting is clearer as one locked transition.
func (s *svc) reserve(spec Spec) (admissionClass, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, ErrFenced
	}

	class := admissionCandidate
	if spec.Advertise {
		class = admissionBackground
		if s.background[spec.SessionID] >= BackgroundProcessLimit {
			return 0, ErrSlotLimit
		}
		s.background[spec.SessionID]++
	} else {
		if s.candidates[spec.SessionID] >= ForegroundCandidateLimit {
			return 0, ErrCandidateLimit
		}
		s.candidates[spec.SessionID]++
	}

	s.live[spec.SessionID]++

	return class, nil
}

func (s *svc) closeAdmission() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
}

func (s *svc) admissionClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.closed
}

func (s *svc) waitForNoLive(ctx context.Context) error {
	for {
		s.mu.Lock()

		live := 0
		for _, count := range s.live {
			live += count
		}
		s.mu.Unlock()

		if live == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("join %d admitted processes: %w", live, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

//nolint:wsl_v5 // The locked helper owns all admission counters.
func (s *svc) releaseReservation(sessionID int64, class admissionClass) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseLocked(sessionID, class)
}

//nolint:wsl_v5 // Counter decrements form one exact-class release.
func (s *svc) releaseLocked(sessionID int64, class admissionClass) {
	if s.live[sessionID] > 0 {
		s.live[sessionID]--
	}
	if class == admissionBackground && s.background[sessionID] > 0 {
		s.background[sessionID]--
	}
	if class == admissionCandidate && s.candidates[sessionID] > 0 {
		s.candidates[sessionID]--
	}
}

func (s *svc) liveCount(sessionID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.live[sessionID]
}

func (s *svc) track(process Process, cancel context.CancelFunc, class admissionClass) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cancels[process.ID] = cancel
	s.liveRecords[process.ID] = process
	s.classes[process.ID] = class
}

func (s *svc) untrack(processID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.cancels, processID)
	delete(s.liveRecords, processID)
	delete(s.classes, processID)
	delete(s.fallbackIntents, processID)
}

func (s *svc) releaseTracked(processID string, sessionID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancels[processID] == nil {
		return
	}

	delete(s.cancels, processID)
	class := s.classes[processID]
	delete(s.liveRecords, processID)
	delete(s.classes, processID)
	delete(s.fallbackIntents, processID)

	s.releaseLocked(sessionID, class)
}

func (s *svc) trackedCancel(processID string) context.CancelFunc {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cancels[processID]
}

func (s *svc) recordFallbackIntent(processID string, intent HostIntent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancels[processID] == nil {
		return
	}

	if s.fallbackIntents[processID] == IntentNone {
		s.fallbackIntents[processID] = intent
	}
}

func (s *svc) trackedCancelWithFallback(
	processID string,
	intent HostIntent,
) context.CancelFunc {
	s.mu.Lock()
	defer s.mu.Unlock()

	cancel := s.cancels[processID]
	if cancel != nil && s.fallbackIntents[processID] == IntentNone {
		s.fallbackIntents[processID] = intent
	}

	return cancel
}

func (s *svc) finalizeTracked(
	ctx context.Context,
	launched *launchResult,
	natural State,
	exitCode *int,
) (Process, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	processID := launched.record.ID
	fallbackIntent := s.fallbackIntents[processID]

	finalized, won, err := s.finalizeWithRetry(
		ctx, processID, fallbackIntent, natural, exitCode, launched.collector.Size(),
	)

	delete(s.cancels, processID)
	class := s.classes[processID]
	delete(s.liveRecords, processID)
	delete(s.classes, processID)
	delete(s.fallbackIntents, processID)

	s.releaseLocked(launched.record.SessionID, class)

	if err != nil {
		err = fmt.Errorf("finalize tracked process: %w", err)
	}

	return finalized, won, err
}

// finalizeWithRetry retries only an uncommitted terminalization transaction.
// A committed transaction already owns its inbox fact and must never be replayed.
func (s *svc) finalizeWithRetry(
	ctx context.Context,
	processID string,
	fallbackIntent HostIntent,
	natural State,
	exitCode *int,
	outputSize int64,
) (Process, bool, error) {
	var finalized Process
	var won bool
	var err error

	for attempt := range 3 {
		if fallbackIntent != IntentNone {
			finalized, won, err = s.store.FinalizeWithIntent(ctx, processID, fallbackIntent, outputSize)
		} else {
			finalized, won, err = s.store.Finalize(ctx, processID, natural, exitCode, outputSize)
		}

		if err == nil {
			return finalized, won, nil
		}

		if attempt == 2 {
			break
		}

		select {
		case <-ctx.Done():
			return Process{}, false, fmt.Errorf("finalize process: %w", ctx.Err())
		case <-time.After(150 * time.Millisecond):
		}
	}

	return Process{}, false, fmt.Errorf("finalize process after retries: %w", err)
}

func (s *svc) cancelTrackedMatching(
	ctx context.Context,
	intent HostIntent,
	matches func(Process) bool,
) (int, error) {
	s.mu.Lock()

	type tracked struct {
		id     string
		cancel context.CancelFunc
	}

	trackedProcesses := make([]tracked, 0, len(s.cancels))
	for processID, cancel := range s.cancels {
		if !matches(s.liveRecords[processID]) {
			continue
		}

		if s.fallbackIntents[processID] == IntentNone {
			s.fallbackIntents[processID] = intent
		}

		trackedProcesses = append(trackedProcesses, tracked{id: processID, cancel: cancel})
	}
	s.mu.Unlock()

	var joinErr error

	for _, process := range trackedProcesses {
		process.cancel()
	}

	for _, process := range trackedProcesses {
		joinErr = errors.Join(joinErr, s.waitForUntracked(ctx, process.id))
	}

	return len(trackedProcesses), joinErr
}
