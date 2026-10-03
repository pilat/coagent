package progressruntime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

var _ Service = (*runtime)(nil)

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
	SetLive(sessionID int64, live Live)
	ReconcileOutputReadiness(ctx context.Context, outputID int64) error
	ReconcileLatestReadiness(ctx context.Context, sessionID int64)
	Wake()
}

type runtime struct {
	sessionStore *sessionstore.Store

	bus    sessionbus.Bus
	liveMu sync.RWMutex
	live   map[int64]Live

	mu             sync.Mutex
	readyOutputs   map[int64]int64
	progressCancel context.CancelFunc
	progressDone   chan struct{}
	progressWake   chan struct{}
}

// Live is the daemon's current runner and context projection.
type Live struct {
	Active  bool
	Working bool
	Context progress.Context
}

func New(store *sessionstore.Store, bus sessionbus.Bus) Service {
	return &runtime{sessionStore: store, bus: bus, live: make(map[int64]Live),
		readyOutputs: make(map[int64]int64), progressWake: make(chan struct{}, 1)}
}

func (r *runtime) SetLive(sessionID int64, live Live) {
	r.liveMu.Lock()
	r.live[sessionID] = live
	r.liveMu.Unlock()
	r.Wake()
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

func (r *runtime) Wake() {
	r.wakeProgress()
}

func (r *runtime) liveState(sessionID int64) Live {
	r.liveMu.RLock()
	defer r.liveMu.RUnlock()
	return r.live[sessionID]
}

func (r *runtime) publish(ctx context.Context, sessionID int64, n sessionevent.Notification) {
	record, err := r.sessionStore.GetSession(ctx, sessionID)
	if err != nil || record.ParentID != 0 {
		return
	}
	owner, _ := record.Attributes["manager_id"].(string)
	r.bus.PublishOwned(sessionID, owner, n)
}
