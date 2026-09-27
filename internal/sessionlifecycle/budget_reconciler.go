package sessionlifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	budgetRescanInterval = 5 * time.Minute
	budgetRetryInterval  = time.Second
)

var _ BudgetReconciler = (*budgetReconciler)(nil)

// BudgetReconciler observes durable budgets independently of model and progress activity.
type BudgetReconciler interface {
	Start(ctx context.Context)
	Stop(ctx context.Context) error
	Wake()
	ReconcileArmed(ctx context.Context) error
}

type budgetTimer interface {
	C() <-chan time.Time
	Reset(time.Duration) bool
	Stop() bool
}

type realBudgetTimer struct{ *time.Timer }

type budgetReconciler struct {
	service budget.Service
	park    func(*sessionstore.BudgetRecord)
	now     func() time.Time
	timer   func(time.Duration) budgetTimer
	wake    chan struct{}
	// Startup scans and the deadline worker share cancellation and join ownership.
	ctx     context.Context //nolint:containedctx // Component lifetime, independent of startup requests.
	cancel  context.CancelFunc
	mu      sync.Mutex
	started bool
	closed  bool
	wg      sync.WaitGroup
	done    chan struct{}
}

// NewBudgetReconciler binds budget observation to the runtime's park operation.
func NewBudgetReconciler(service budget.Service, park func(*sessionstore.BudgetRecord)) BudgetReconciler {
	ctx, cancel := context.WithCancel(context.Background())

	return &budgetReconciler{
		service: service, park: park, now: time.Now, timer: newRealBudgetTimer,
		wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
}

func (r *budgetReconciler) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.started || r.closed {
		return
	}

	r.started = true

	r.wg.Add(1)
	go r.run(logger.ToContext(r.ctx, logger.Ctx(ctx))) //nolint:contextcheck // Worker outlives startup requests.
}

func (r *budgetReconciler) Stop(ctx context.Context) error {
	r.mu.Lock()
	if !r.closed {
		r.closed = true

		r.cancel()
		go func() {
			r.wg.Wait()
			close(r.done)
		}()
	}
	r.mu.Unlock()

	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("join budget reconciler: %w", ctx.Err())
	}
}

func (r *budgetReconciler) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *budgetReconciler) ReconcileArmed(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrShuttingDown
	}

	r.wg.Add(1)

	r.mu.Unlock()
	defer r.wg.Done()

	scanCtx, cancel := context.WithCancel(ctx)

	stop := context.AfterFunc(r.ctx, cancel) //nolint:contextcheck // Shutdown also cancels request-owned scans.
	defer stop()
	defer cancel()

	_, err := r.reconcile(scanCtx, r.now().UTC())

	return err
}

func (t *realBudgetTimer) C() <-chan time.Time { return t.Timer.C }

func newRealBudgetTimer(delay time.Duration) budgetTimer {
	return &realBudgetTimer{Timer: time.NewTimer(delay)}
}

func (r *budgetReconciler) run(ctx context.Context) {
	defer r.wg.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Ctx(ctx).Named("sessionlifecycle.budget").Error(
				"worker_panic", zap.Any("panic", recovered), zap.Stack("stack"),
			)
		}
	}()

	timer := r.timer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
			if !timer.Stop() {
				select {
				case <-timer.C():
				default:
				}
			}

			timer.Reset(0)
		case <-timer.C():
			timer.Reset(r.reconcileSafely(ctx))
		}
	}
}

//nolint:nonamedreturns // Panic recovery must return a bounded retry delay.
func (r *budgetReconciler) reconcileSafely(ctx context.Context) (delay time.Duration) {
	log := logger.Ctx(ctx).Named("sessionlifecycle.budget")

	defer func() {
		if recovered := recover(); recovered != nil {
			delay = budgetRetryInterval

			log.Error("reconcile_panic", zap.Any("panic", recovered), zap.Stack("stack"))
		}
	}()

	next, err := r.reconcile(ctx, r.now().UTC())
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("reconcile_failed", zap.Error(err))
		}

		return budgetRetryInterval
	}

	return max(next, budgetRetryInterval)
}

func (r *budgetReconciler) reconcile(ctx context.Context, now time.Time) (time.Duration, error) {
	armed, err := r.service.ListArmed(ctx)
	if err != nil {
		return budgetRetryInterval, fmt.Errorf("list armed budgets: %w", err)
	}

	next := budgetRescanInterval
	var failures []error

	for _, armedRecord := range armed {
		if err := ctx.Err(); err != nil {
			return budgetRetryInterval, fmt.Errorf("reconcile budgets: %w", err)
		}

		// Observe reads usage and arbitrates a crossing inside its own transaction.
		record, fired, err := r.service.Observe(ctx, armedRecord.RootSessionID, 0, now, "")
		if err != nil {
			failures = append(failures, fmt.Errorf("observe budget for session %d: %w", armedRecord.RootSessionID, err))
			continue
		}

		if fired && record != nil && record.State == sessionstore.BudgetFired &&
			record.ParkPhase == "requested" && ctx.Err() == nil {
			r.park(record)
		}

		if record != nil && record.State == sessionstore.BudgetArmed && record.DurationSeconds != nil {
			deadline := record.ArmedAt.Add(time.Duration(*record.DurationSeconds) * time.Second)
			next = min(next, max(deadline.Sub(now), budgetRetryInterval))
		}
	}

	return next, errors.Join(failures...)
}
