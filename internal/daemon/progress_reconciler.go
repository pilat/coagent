//nolint:wrapcheck // Reconciler returns durable outbox/store errors unchanged.; nosemgrep: semgrep.coagent-no-preamble-before-package
package daemon

import (
	"context"
	"errors"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/sessionstore"
)

const progressSilenceInterval = 5 * time.Minute

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
func (s *svc) startProgressReconciler(ctx context.Context) {
	store, ok := s.sessionStore.(sessionstore.ProgressStore)
	if !ok || s.progressDone != nil {
		return
	}

	reconcileCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.progressCancel = cancel

	s.progressDone = make(chan struct{})
	go func() {
		defer close(s.progressDone)
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Ctx(reconcileCtx).Named("daemon.progress").Error(
					"reconciler_panic", zap.Any("panic", recovered), zap.Stack("stack"),
				)
			}
		}()

		timer := s.progressTimer(0)
		defer timer.Stop()

		for {
			select {
			case <-reconcileCtx.Done():
				return
			case <-s.progressWake:
				if !timer.Stop() {
					select {
					case <-timer.C():
					default:
					}
				}
				timer.Reset(0)
			case now := <-timer.C():
				delay := max(s.reconcileProgressSafely(reconcileCtx, store, now.UTC()), time.Second)

				timer.Reset(delay)
			}
		}
	}()
}

// reconcileProgressSafely confines one tick's panic to that tick: the
// reconciler keeps serving later deadlines instead of dying silently.
func (s *svc) reconcileProgressSafely(
	ctx context.Context,
	store sessionstore.ProgressStore,
	now time.Time,
) time.Duration {
	var delay time.Duration

	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Ctx(ctx).Named("daemon.progress").Error(
					"reconciler_panic", zap.Any("panic", recovered), zap.Stack("stack"),
				)

				delay = progressSilenceInterval
			}
		}()

		delay = s.reconcileProgress(ctx, store, now)
	}()

	return delay
}

func (s *svc) wakeProgress() {
	select {
	case s.progressWake <- struct{}{}:
	default:
	}
}

func (s *svc) reconcileProgress(
	ctx context.Context,
	store sessionstore.ProgressStore,
	now time.Time,
) time.Duration {
	rootIDs, err := store.ListAutonomousProgressRoots(ctx)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.progress").Warn("list_roots_failed", zap.Error(err))
		return progressSilenceInterval
	}

	next := progressSilenceInterval

	for _, rootID := range rootIDs {
		facts, captureErr := store.CaptureProgress(ctx, rootID)
		if captureErr != nil {
			logger.Ctx(ctx).Named("daemon.progress").Warn("capture_failed",
				zap.Int64("session_id", rootID), zap.Error(captureErr))

			continue
		}

		if s.budgetSvc != nil {
			record, fired, budgetErr := s.budgetSvc.Observe(ctx, rootID, facts.CostUSD, now, "")
			if budgetErr != nil {
				logger.Ctx(ctx).Named("daemon.progress").Warn("budget_observe_failed",
					zap.Int64("session_id", rootID), zap.Error(budgetErr))
			} else if fired {
				s.startBudgetPark(record)
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

		baseline := facts.EpisodeStartedAt
		if facts.LastSemanticOutputAt != nil && (baseline == nil || facts.LastSemanticOutputAt.After(*baseline)) {
			baseline = facts.LastSemanticOutputAt
		}

		if baseline == nil {
			continue
		}

		deadline := baseline.Add(progressSilenceInterval)
		if now.Before(deadline) {
			next = min(next, deadline.Sub(now))
			continue
		}

		if err := s.enqueueProgressSilence(ctx, facts, deadline, now); err != nil {
			logger.Ctx(ctx).Named("daemon.progress").Warn("enqueue_failed",
				zap.Int64("session_id", rootID), zap.Error(err))
		}
	}

	return next
}

//nolint:wsl_v5 // Identity, render, and enqueue are one reconciliation path.
func (s *svc) enqueueProgressSilence(
	ctx context.Context,
	facts *sessionstore.ProgressFacts,
	deadline, observedAt time.Time,
) error {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()

	sourceKey := "progress:silence:" + strconv.FormatInt(facts.OutboxWatermark, 10) + ":" +
		strconv.FormatInt(deadline.Unix(), 10) + ":g" + strconv.FormatInt(facts.ModelInputGeneration, 10)
	if _, ok, err := s.existingProgressOutput(ctx, facts.RootID, sourceKey); err != nil {
		return err
	} else if ok {
		return nil
	}

	snapshot, err := s.progressSnapshot(facts, observedAt)
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

	store, ok := s.sessionStore.(sessionstore.ProgressStore)
	if !ok {
		return errors.New("progress output store unavailable")
	}

	// Silence waits belong to the transition that captured them: a superseded
	// silence card is simply dropped because the newer transition owns the next
	// card. No recapture retry — the reconciler re-derives the deadline anyway.
	if _, err := store.EnqueueProgressOutput(
		ctx, draft, facts.ModelInputGeneration, facts.Status,
	); err != nil && !errors.Is(err, sessionstore.ErrProgressSuperseded) {
		return err
	}

	return nil
}

func (s *svc) enqueueProgressChange(ctx context.Context, rootID int64) (string, bool, error) {
	store, ok := s.sessionStore.(sessionstore.ProgressStore)
	if !ok {
		return "", false, errors.New("progress store unavailable")
	}

	facts, err := store.CaptureProgress(ctx, rootID)
	if err != nil {
		return "", false, err
	}

	return s.enqueueProgressChangeFacts(
		ctx, facts, "message:"+strconv.FormatInt(facts.MessageWatermark, 10), true,
	)
}

func (s *svc) enqueueProgressChangeFacts(
	ctx context.Context,
	facts *sessionstore.ProgressFacts,
	causalID string,
	recaptureOnSuperseded bool,
) (string, bool, error) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()

	content, published, err := s.tryEnqueueProgressChange(ctx, facts, causalID)
	if err == nil || !errors.Is(err, sessionstore.ErrProgressSuperseded) || !recaptureOnSuperseded {
		return content, published, err
	}

	store, ok := s.sessionStore.(sessionstore.ProgressStore)
	if !ok {
		return "", false, errors.New("progress store unavailable")
	}

	fresh, captureErr := store.CaptureProgress(ctx, facts.RootID)
	if captureErr != nil {
		return "", false, captureErr
	}

	return s.tryEnqueueProgressChange(ctx, fresh, causalID)
}

func (s *svc) tryEnqueueProgressChange(
	ctx context.Context,
	facts *sessionstore.ProgressFacts,
	causalID string,
) (string, bool, error) {
	sourceKey := "progress:change:" + causalID +
		":g" + strconv.FormatInt(facts.ModelInputGeneration, 10)
	if existing, ok, err := s.existingProgressOutput(ctx, facts.RootID, sourceKey); err != nil {
		return "", false, err
	} else if ok {
		return existing.Content, true, nil
	}

	snapshot, err := s.progressSnapshot(facts, s.progressNow().UTC())
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

	store, ok := s.sessionStore.(sessionstore.ProgressStore)
	if !ok {
		return "", false, errors.New("progress output store unavailable")
	}

	if _, err := store.EnqueueProgressOutput(
		ctx, draft, facts.ModelInputGeneration, facts.Status,
	); err != nil {
		return "", false, err
	}

	return content, true, nil
}

//nolint:wsl_v5 // Optional capability and sentinel handling remain adjacent.
func (s *svc) existingProgressOutput(
	ctx context.Context,
	rootID int64,
	sourceKey string,
) (*sessionstore.OutputRecord, bool, error) {
	store, ok := s.sessionStore.(sessionstore.OutputIdentityStore)
	if !ok {
		return nil, false, nil
	}
	record, err := store.OutputBySourceKey(ctx, rootID, sourceKey)
	if errors.Is(err, sessionstore.ErrNoOutput) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	return record, true, nil
}
