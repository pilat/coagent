package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/session"
)

var errProgressUnavailable = errors.New("progress runtime unavailable")

func (s *svc) CurrentProgress(ctx context.Context, rootID int64) (*controllerapi.ProgressData, error) {
	if s.progress == nil {
		return nil, errProgressUnavailable
	}

	current, err := s.progress.Current(ctx, rootID)
	if err != nil {
		return nil, fmt.Errorf("current progress: %w", err)
	}

	return current, nil
}

func (s *svc) RefreshProgress(ctx context.Context, rootID int64) error {
	if s.progress == nil {
		return errProgressUnavailable
	}

	if err := s.progress.Refresh(ctx, rootID); err != nil {
		return fmt.Errorf("refresh progress: %w", err)
	}

	return nil
}

func (s *svc) renderFinalOutput(ctx context.Context, rootID int64, text string) (string, error) {
	if s.progress == nil {
		return text, nil
	}

	rendered, err := s.progress.RenderFinal(ctx, rootID, text)
	if err != nil {
		return "", fmt.Errorf("render final progress: %w", err)
	}

	return rendered, nil
}

func (s *svc) enqueueProgressChange(ctx context.Context, rootID int64) (string, bool, error) {
	if s.progress == nil {
		return "", false, errProgressUnavailable
	}

	content, published, err := s.progress.EnqueueChange(ctx, rootID)
	if err != nil {
		return "", false, fmt.Errorf("enqueue progress change: %w", err)
	}

	return content, published, nil
}

func (s *svc) enqueueProgressChangeFor(
	ctx context.Context,
	rootID int64,
	causalID string,
	recaptureOnSuperseded bool,
) (string, bool, error) {
	if s.progress == nil {
		return "", false, errProgressUnavailable
	}

	content, published, err := s.progress.EnqueueChangeFor(ctx, rootID, causalID, recaptureOnSuperseded)
	if err != nil {
		return "", false, fmt.Errorf("enqueue causal progress change: %w", err)
	}

	return content, published, nil
}

func (s *svc) startProgressReconciler(ctx context.Context) {
	if s.progress != nil {
		s.progress.Start(ctx)
	}
}

//nolint:wsl_v5 // Runtime lookup is guarded at each ownership boundary.
func (s *svc) liveContextProjection(ctx context.Context, rootID int64) (session.ContextProjection, bool) {
	s.mu.Lock()
	runner := s.loops[rootID]
	s.mu.Unlock()
	if runner == nil {
		return session.ContextProjection{}, false
	}

	runner.svcMu.Lock()
	service := runner.service
	runner.svcMu.Unlock()
	if service == nil {
		return session.ContextProjection{}, false
	}

	provider, ok := service.(interface {
		ContextProjection(context.Context) session.ContextProjection
	})
	if !ok {
		return session.ContextProjection{}, false
	}

	return provider.ContextProjection(ctx), true
}

func newProgressRuntime(
	store progressruntime.Store,
	budgetSvc budget.Service,
	daemon *svc,
) progressruntime.Service {
	if store == nil {
		return nil
	}

	return progressruntime.New(
		store, budgetSvc, daemon.HasActiveLoop, daemon.liveContextProjection, daemon.startBudgetPark,
	)
}
