//nolint:wrapcheck // Reconciler returns durable outbox/store errors unchanged.; nosemgrep: semgrep.coagent-no-preamble-before-package
package progressruntime

import (
	"context"
	"errors"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	// MainModelProgressInterval is the maximum quiet period while the root loop is working.
	MainModelProgressInterval = 30 * time.Second
	// SilenceInterval is the maximum quiet period for autonomous work without an active root loop.
	SilenceInterval = 5 * time.Minute
)

type progressTimer interface {
	C() <-chan time.Time
	Reset(time.Duration) bool
	Stop() bool
}

type realProgressTimer struct{ *time.Timer }

func newRealProgressTimer(delay time.Duration) progressTimer {
	return &realProgressTimer{Timer: time.NewTimer(delay)}
}

func (t *realProgressTimer) C() <-chan time.Time { return t.Timer.C }

//nolint:wsl_v5 // Timer reset branches are one scheduling protocol.
func (r *runtime) startProgressReconciler(ctx context.Context) {
	r.mu.Lock()
	if r.progressDone != nil {
		r.mu.Unlock()
		return
	}

	reconcileCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.progressCancel = cancel
	r.progressDone = make(chan struct{})
	done := r.progressDone
	r.mu.Unlock()

	go func() {
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Ctx(reconcileCtx).Named("progressruntime.reconciler").Error(
					"reconciler_panic", zap.Any("panic", recovered), zap.Stack("stack"),
				)
			}
		}()

		timer := r.progressTimer(0)
		defer timer.Stop()

		for {
			select {
			case <-reconcileCtx.Done():
				return
			case <-r.progressWake:
				if !timer.Stop() {
					select {
					case <-timer.C():
					default:
					}
				}
				timer.Reset(0)
			case now := <-timer.C():
				delay := max(r.reconcileProgressSafely(reconcileCtx, now.UTC()), time.Second)

				timer.Reset(delay)
			}
		}
	}()
}

// reconcileProgressSafely confines one tick's panic to that tick: the
// reconciler keeps serving later deadlines instead of dying silently.
func (r *runtime) reconcileProgressSafely(
	ctx context.Context,
	now time.Time,
) time.Duration {
	var delay time.Duration

	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Ctx(ctx).Named("progressruntime.reconciler").Error(
					"reconciler_panic", zap.Any("panic", recovered), zap.Stack("stack"),
				)

				delay = SilenceInterval
			}
		}()

		delay = r.reconcileProgress(ctx, now)
	}()

	return delay
}

func (r *runtime) wakeProgress() {
	select {
	case r.progressWake <- struct{}{}:
	default:
	}
}

func (r *runtime) reconcileProgress(
	ctx context.Context,
	now time.Time,
) time.Duration {
	rootIDs, err := r.sessionStore.ListAutonomousProgressRoots(ctx)
	if err != nil {
		logger.Ctx(ctx).Named("progressruntime.reconciler").Warn("list_roots_failed", zap.Error(err))
		return SilenceInterval
	}

	next := SilenceInterval

	for _, rootID := range rootIDs {
		facts, captureErr := r.sessionStore.CaptureProgress(ctx, rootID)
		if captureErr != nil {
			logger.Ctx(ctx).Named("progressruntime.reconciler").Warn("capture_failed",
				zap.Int64("session_id", rootID), zap.Error(captureErr))

			continue
		}

		if r.budgetSvc != nil {
			record, fired, budgetErr := r.budgetSvc.Observe(ctx, rootID, facts.CostUSD, now, "")
			if budgetErr != nil {
				logger.Ctx(ctx).Named("progressruntime.reconciler").Warn("budget_observe_failed",
					zap.Int64("session_id", rootID), zap.Error(budgetErr))
			} else if fired {
				r.startBudgetPark(record)
				continue
			}
		}

		if facts.Budget != nil && facts.Budget.State == sessionstore.BudgetArmed &&
			facts.Budget.DurationSeconds != nil {
			budgetDeadline := facts.Budget.ArmedAt.Add(
				time.Duration(*facts.Budget.DurationSeconds) * time.Second,
			)
			if now.Before(budgetDeadline) {
				next = min(next, budgetDeadline.Sub(now))
			}
		}

		interval := SilenceInterval
		if r.mainModelWorking(rootID) {
			interval = MainModelProgressInterval
		}

		next = min(next, interval)

		baseline := facts.EpisodeStartedAt
		if facts.LastSemanticOutputAt != nil && (baseline == nil || facts.LastSemanticOutputAt.After(*baseline)) {
			baseline = facts.LastSemanticOutputAt
		}

		if baseline == nil {
			continue
		}

		deadline := baseline.Add(interval)
		if now.Before(deadline) {
			next = min(next, deadline.Sub(now))
			continue
		}

		if err := r.enqueueProgressSilence(ctx, facts, deadline, now); err != nil {
			logger.Ctx(ctx).Named("progressruntime.reconciler").Warn("enqueue_failed",
				zap.Int64("session_id", rootID), zap.Error(err))
		}
	}

	return next
}

//nolint:wsl_v5 // Identity, render, and enqueue are one reconciliation path.
func (r *runtime) enqueueProgressSilence(
	ctx context.Context,
	facts *sessionstore.ProgressFacts,
	deadline, observedAt time.Time,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	sourceKey := "progress:silence:" + strconv.FormatInt(facts.OutboxWatermark, 10) + ":" +
		strconv.FormatInt(deadline.Unix(), 10) + ":g" + strconv.FormatInt(facts.ModelInputGeneration, 10)
	if _, ok, err := r.existingProgressOutput(ctx, facts.RootID, sourceKey); err != nil {
		return err
	} else if ok {
		return nil
	}

	snapshot, err := r.progressSnapshot(facts, observedAt)
	if err != nil {
		return err
	}
	attributes := map[string]any{"progress_revision": snapshot.Revision}
	draft := sessionstore.OutputDraft{
		SessionID: facts.RootID, Type: sessionstore.OutputMessageReplaceable,
		Content: progress.RenderCompact(snapshot, logger.Redact), Attributes: attributes, SourceKey: sourceKey,
		CreatedAt: observedAt,
	}
	draft.Fingerprint = sessionstore.OutputFingerprint(draft.Type, draft.Content, draft.SessionID, attributes)

	// Silence waits belong to the transition that captured them: a superseded
	// silence card is simply dropped because the newer transition owns the next
	// card. No recapture retry — the reconciler re-derives the deadline anyway.
	if _, err := r.sessionStore.EnqueueProgressOutput(
		ctx, draft, facts.ModelInputGeneration, facts.Status,
	); err != nil && !errors.Is(err, sessionstore.ErrProgressSuperseded) {
		return err
	} else if err == nil {
		r.publish(facts.RootID, sessionevent.Notification{
			Type: sessionevent.NotifyMessage, Message: draft.Content,
		})
	}

	return nil
}

func (r *runtime) enqueueProgressChange(ctx context.Context, rootID int64) (string, bool, error) {
	facts, err := r.sessionStore.CaptureProgress(ctx, rootID)
	if err != nil {
		return "", false, err
	}

	return r.enqueueProgressChangeFacts(
		ctx, facts, "message:"+strconv.FormatInt(facts.MessageWatermark, 10), true,
	)
}

func (r *runtime) enqueueProgressChangeFacts(
	ctx context.Context,
	facts *sessionstore.ProgressFacts,
	causalID string,
	recaptureOnSuperseded bool,
) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	content, published, err := r.tryEnqueueProgressChange(ctx, facts, causalID)
	if err == nil || !errors.Is(err, sessionstore.ErrProgressSuperseded) || !recaptureOnSuperseded {
		return content, published, err
	}

	fresh, captureErr := r.sessionStore.CaptureProgress(ctx, facts.RootID)
	if captureErr != nil {
		return "", false, captureErr
	}

	return r.tryEnqueueProgressChange(ctx, fresh, causalID)
}

func (r *runtime) tryEnqueueProgressChange(
	ctx context.Context,
	facts *sessionstore.ProgressFacts,
	causalID string,
) (string, bool, error) {
	sourceKey := "progress:change:" + causalID +
		":g" + strconv.FormatInt(facts.ModelInputGeneration, 10)
	if existing, ok, err := r.existingProgressOutput(ctx, facts.RootID, sourceKey); err != nil {
		return "", false, err
	} else if ok {
		return existing.Content, true, nil
	}

	snapshot, err := r.progressSnapshot(facts, r.progressNow().UTC())
	if err != nil {
		return "", false, err
	}

	content := progress.RenderCompact(snapshot, logger.Redact)
	attributes := map[string]any{"progress_revision": snapshot.Revision}
	draft := sessionstore.OutputDraft{
		SessionID: facts.RootID, Type: sessionstore.OutputMessageReplaceable, Content: content,
		Attributes: attributes,
		SourceKey:  sourceKey,
		CreatedAt:  snapshot.ObservedAt,
	}
	draft.Fingerprint = sessionstore.OutputFingerprint(draft.Type, draft.Content, facts.RootID, attributes)

	if _, err := r.sessionStore.EnqueueProgressOutput(
		ctx, draft, facts.ModelInputGeneration, facts.Status,
	); err != nil {
		return "", false, err
	}

	return content, true, nil
}
