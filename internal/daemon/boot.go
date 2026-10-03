package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

type recovery struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	closed bool
}

func (r *recovery) Start(ctx context.Context, run func(context.Context)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed || r.done != nil {
		return false
	}

	recoveryCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	r.cancel = cancel
	r.done = done

	go runRecovery(recoveryCtx, done, run)

	return true
}

func (r *recovery) Close() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
	if r.cancel != nil {
		r.cancel()
	}

	return r.done
}

func (r *recovery) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.done != nil
}

// Start re-establishes in-flight children and re-delivers undelivered completions
// after a restart. Only PASS 0 blocks; the resumes run asynchronously.
func (s *svc) Start(ctx context.Context) error {
	finishProcessRecovery := s.beginProcessRecovery()
	defer finishProcessRecovery()

	if s.shuttingDown.Load() {
		return errDaemonShuttingDown
	}

	// Must precede the sweep: a session left mid-clear or mid-kill by the previous
	// run would otherwise be resumed in that half-torn state.
	if err := s.finishInterruptedKills(ctx); err != nil {
		return fmt.Errorf("finish interrupted kills: %w", err)
	}

	// /stop is a durable two-phase park. If the process died after writing
	// stopping, finish the same idempotent operation before any recovery sweep can
	// restart work from that tree. An explicit stop whose terminal output is still
	// owed converges to the same completion transaction; other stopping trees
	// finish silently as before.
	records, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for stop recovery: %w", err)
	}

	stopping := make(map[int64]bool)

	for _, rec := range records {
		if rec.Status == sessionstore.SessionStatusStopping {
			stopping[rec.ID] = true
		}
	}

	owedStops := make(map[int64]sessionstore.InterruptedExplicitStop)

	stops, selectErr := s.store.SelectInterruptedExplicitStops(ctx)
	if selectErr != nil {
		return fmt.Errorf("select interrupted explicit stops: %w", selectErr)
	}

	for _, stop := range stops {
		owedStops[stop.SessionID] = stop
	}

	// Startup interruption sweep for background Bash processes runs after the
	// stop-fence recovery below: a root mid-stop must finish its operator
	// fence first, so records it covers stay cancelled with no wake event.
	return s.finishStoppingRoots(ctx, records, stopping, owedStops, func() error {
		if err := s.recoverProcessInterruptions(ctx); err != nil {
			return fmt.Errorf("recover background process interruptions: %w", err)
		}

		return nil
	})
}

func (s *svc) beginProcessRecovery() func() {
	done := make(chan struct{})

	s.processRecoveryMu.Lock()
	s.processRecovery = done
	s.processRecoveryMu.Unlock()

	return func() {
		close(done)

		s.processRecoveryMu.Lock()
		if s.processRecovery == done {
			s.processRecovery = nil
		}
		s.processRecoveryMu.Unlock()
	}
}

func (s *svc) currentProcessRecovery() <-chan struct{} {
	s.processRecoveryMu.Lock()
	defer s.processRecoveryMu.Unlock()

	return s.processRecovery
}

func newRecovery() *recovery { return &recovery{} }

func runRecovery(ctx context.Context, done chan<- struct{}, run func(context.Context)) {
	defer close(done)
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Ctx(ctx).Named("daemon.recovery").Error(
				"recovery_panic", zap.Any("recovered", recovered), zap.Stack("stack"),
			)
		}
	}()

	run(ctx)
}

func (s *svc) recoveredStopCancellationCount(
	ctx context.Context,
	rootID int64,
	since time.Time,
) (int, error) {
	count, err := s.processStore.CountTerminalByIntentSince(
		ctx, rootID, backgroundprocess.IntentSessionStopped, since,
	)
	if err != nil {
		return 0, fmt.Errorf("count stopped processes for session %d: %w", rootID, err)
	}

	return count, nil
}

// finishInterruptedKills completes a /kill or /clear whose process died
// between the durable terminating fence and the final transition: the store
// kills the root (emitting its close output when no replacement took over),
// then the same tree cleanup as a live Kill reruns idempotently.
func (s *svc) finishInterruptedKills(ctx context.Context) error {
	records, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for kill recovery: %w", err)
	}

	interrupted := make([]*sessionstore.SessionRecord, 0)

	for _, rec := range records {
		if rec.Status == sessionstore.SessionStatusTerminating && rec.KilledAt == nil {
			interrupted = append(interrupted, rec)
		}
	}

	if len(interrupted) == 0 {
		return nil
	}

	cleanupCtx := context.WithoutCancel(ctx)

	for _, record := range interrupted {
		if _, err := s.cancelSessionSubtreeProcesses(
			cleanupCtx, record.ID, backgroundprocess.IntentSessionKilled,
		); err != nil {
			return fmt.Errorf("cancel processes for interrupted kill %d: %w", record.ID, err)
		}
	}

	for _, record := range interrupted {
		cancelled, err := s.processStore.CountTerminalByIntentSince(
			cleanupCtx, record.ID, backgroundprocess.IntentSessionKilled, record.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("count processes for interrupted kill %d: %w", record.ID, err)
		}

		if _, err := s.store.MarkSessionKilledWithOutput(
			cleanupCtx, record.ID, cancelled,
		); err != nil {
			return fmt.Errorf("finish killed session %d: %w", record.ID, err)
		}

		s.removeSchedules(cleanupCtx, record.ID)
		s.cascadeKillChildrenForKilledTree(
			cleanupCtx, record.ID, 0, time.Now().Add(cascadeRetryBudget),
		)
	}

	return nil
}

// recoverProcessInterruptions terminalizes leftover processes. Finalization
// atomically records any owed inbox fact for ordinary input recovery.
func (s *svc) recoverProcessInterruptions(ctx context.Context) error {
	if _, err := s.processSvc.InterruptNonterminal(ctx); err != nil {
		return fmt.Errorf("interrupt nonterminal processes: %w", err)
	}

	return nil
}

// finishStoppingRoots completes every interrupted stop before ordinary resume:
// stopping trees finish their cleanup (and owed terminal output), while a
// stopped root whose fence committed through the non-explicit fallback gets the
// idempotent terminal transaction so its start receipt never strands.
func (s *svc) finishStoppingRoots(
	ctx context.Context,
	records []*sessionstore.SessionRecord,
	stopping map[int64]bool,
	owedStops map[int64]sessionstore.InterruptedExplicitStop,
	afterStops func() error,
) error {
	if err := s.recoverStoppingSessions(ctx, records, stopping, owedStops); err != nil {
		return err
	}

	if err := s.convergeStoppedSessions(ctx, records, owedStops); err != nil {
		return err
	}

	if afterStops != nil {
		if err := afterStops(); err != nil {
			return err
		}
	}

	return s.finishRecoveredServices(ctx)
}

func (s *svc) recoverStoppingSessions(
	ctx context.Context,
	records []*sessionstore.SessionRecord,
	stopping map[int64]bool,
	owedStops map[int64]sessionstore.InterruptedExplicitStop,
) error {
	for _, rec := range records {
		if !stopping[rec.ID] || stopping[rec.ParentID] {
			continue
		}

		if stop, owed := owedStops[rec.ID]; owed {
			if err := s.stopTreeCleanup(ctx, rec.ID, stopTreeOptions{keepRootStopping: true}); err != nil {
				return fmt.Errorf("recover stopping session %d: %w", rec.ID, err)
			}

			cancelled, err := s.recoveredStopCancellationCount(ctx, rec.ID, stop.ReceivedAt)
			if err != nil {
				return err
			}

			if err := s.completeExplicitStop(ctx, rec.ID, stop.InputID, cancelled); err != nil {
				return fmt.Errorf("recover explicit stop for session %d: %w", rec.ID, err)
			}

			continue
		}

		if err := s.stopTreeCleanup(ctx, rec.ID, stopTreeOptions{}); err != nil {
			return fmt.Errorf("recover stopping session %d: %w", rec.ID, err)
		}
	}

	return nil
}

func (s *svc) convergeStoppedSessions(
	ctx context.Context,
	records []*sessionstore.SessionRecord,
	owedStops map[int64]sessionstore.InterruptedExplicitStop,
) error {
	for _, rec := range records {
		if rec.Status != sessionstore.SessionStatusStopped {
			continue
		}

		if stop, owed := owedStops[rec.ID]; owed {
			cancelled, err := s.recoveredStopCancellationCount(ctx, rec.ID, stop.ReceivedAt)
			if err != nil {
				return err
			}

			if err := s.completeExplicitStop(ctx, rec.ID, stop.InputID, cancelled); err != nil {
				return fmt.Errorf("converge explicit stop for session %d: %w", rec.ID, err)
			}
		}
	}

	return nil
}

func (s *svc) finishRecoveredServices(ctx context.Context) error {
	if err := s.reconcileArmedBudgets(ctx); err != nil {
		return err
	}

	pending, parkErr := s.budgetSvc.ListPendingParks(ctx)
	if parkErr != nil {
		return fmt.Errorf("list pending budget parks: %w", parkErr)
	}

	for _, record := range pending {
		s.parkBudgetTree(ctx, record)
	}

	// PASS 0 is the one blocking phase. Controllers and the schedule executor start
	// the moment Start returns, and a runner they open makes it skip that session.
	if err := s.recoverOrphanedCalls(ctx); err != nil {
		return err
	}

	if err := s.recoverInterruptedTools(ctx); err != nil {
		return err
	}

	s.startInboxWake(ctx)
	s.progress.Start(ctx)

	s.startRecovery(ctx)

	return nil
}

func (s *svc) startRecovery(ctx context.Context) {
	if s.shuttingDown.Load() {
		return
	}

	s.recovery.Start(ctx, s.resumeAfterRestart)
}
