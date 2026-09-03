package progressruntime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

// Store is the durable progress projection and output identity boundary.
type Store interface {
	sessionstore.ProgressStore
	sessionstore.ReadinessStore
}

// Service owns progress projection, durable cards, and silence reconciliation.
type Service interface {
	Start(ctx context.Context)
	Stop(ctx context.Context) error
	Current(ctx context.Context, rootID int64) (*controllerapi.ProgressData, error)
	Refresh(ctx context.Context, rootID int64) error
	RenderFinal(ctx context.Context, rootID int64, text string) (string, error)
	EnqueueChange(ctx context.Context, rootID int64) (string, bool, error)
	EnqueueChangeFor(
		ctx context.Context,
		rootID int64,
		causalID string,
		recaptureOnSuperseded bool,
	) (string, bool, error)
	Reconcile(ctx context.Context, now time.Time) time.Duration
	ReconcileArmedBudgets(ctx context.Context) error
	ReconcileOutputReadiness(ctx context.Context, outputID int64) error
	ReconcileLatestReadiness(ctx context.Context, sessionID int64)
	Wake()
}

var _ Service = (*runtime)(nil)

type runtime struct {
	sessionStore Store
	budgetSvc    budget.Service

	hasActiveLoop         func(int64) bool
	mainModelWorking      func(int64) bool
	liveContextProjection func(context.Context, int64) (progress.Context, bool)
	startBudgetPark       func(*sessionstore.BudgetRecord)
	publish               func(int64, sessionevent.Notification)

	mu             sync.Mutex
	progressCancel context.CancelFunc
	progressDone   chan struct{}
	progressWake   chan struct{}
	progressNow    func() time.Time
	progressTimer  func(time.Duration) progressTimer
}

func New(
	store Store,
	budgetSvc budget.Service,
	hasActiveLoop func(int64) bool,
	mainModelWorking func(int64) bool,
	contextProjection func(context.Context, int64) (progress.Context, bool),
	startBudgetPark func(*sessionstore.BudgetRecord),
	publish func(int64, sessionevent.Notification),
) Service {
	return &runtime{
		sessionStore: store, budgetSvc: budgetSvc,
		hasActiveLoop: hasActiveLoop, mainModelWorking: mainModelWorking,
		liveContextProjection: contextProjection,
		startBudgetPark:       startBudgetPark,
		publish:               publish,
		progressWake:          make(chan struct{}, 1), progressNow: time.Now, progressTimer: newRealProgressTimer,
	}
}

func (r *runtime) Start(ctx context.Context) {
	r.startProgressReconciler(ctx)
}

func (r *runtime) Stop(ctx context.Context) error {
	r.mu.Lock()
	cancel, done := r.progressCancel, r.progressDone
	r.mu.Unlock()

	if cancel == nil || done == nil {
		return nil
	}

	cancel()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *runtime) Current(ctx context.Context, rootID int64) (*controllerapi.ProgressData, error) {
	return r.current(ctx, rootID)
}

func (r *runtime) Refresh(ctx context.Context, rootID int64) error {
	return r.refresh(ctx, rootID)
}

func (r *runtime) RenderFinal(ctx context.Context, rootID int64, text string) (string, error) {
	return r.renderFinalOutput(ctx, rootID, text)
}

func (r *runtime) EnqueueChange(ctx context.Context, rootID int64) (string, bool, error) {
	content, published, err := r.enqueueProgressChange(ctx, rootID)
	if err == nil {
		r.wakeProgress()
	}

	return content, published, err
}

func (r *runtime) EnqueueChangeFor(
	ctx context.Context,
	rootID int64,
	causalID string,
	recaptureOnSuperseded bool,
) (string, bool, error) {
	facts, err := r.sessionStore.CaptureProgress(ctx, rootID)
	if err != nil {
		return "", false, fmt.Errorf("capture progress: %w", err)
	}

	content, published, err := r.enqueueProgressChangeFacts(ctx, facts, causalID, recaptureOnSuperseded)
	if err == nil {
		r.wakeProgress()
	}

	return content, published, err
}

func (r *runtime) Reconcile(ctx context.Context, now time.Time) time.Duration {
	return r.reconcileProgressSafely(ctx, now)
}

func (r *runtime) ReconcileArmedBudgets(ctx context.Context) error {
	return r.reconcileArmedBudgets(ctx)
}

func (r *runtime) Wake() {
	r.wakeProgress()
}
