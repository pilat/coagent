package daemon

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
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

func (s *svc) liveContextProjection(ctx context.Context, rootID int64) (progress.Context, bool) {
	activeRunner, ok := s.runners.Load(rootID)
	if !ok {
		return progress.Context{}, false
	}

	service := activeRunner.Service()
	if service == nil {
		return progress.Context{}, false
	}

	provider, ok := service.(interface {
		ContextProjection(context.Context) progress.Context
	})
	if !ok {
		return progress.Context{}, false
	}

	return provider.ContextProjection(ctx), true
}

func (s *svc) mainModelWorking(rootID int64) bool {
	activeRunner, ok := s.runners.Load(rootID)
	if !ok {
		return false
	}

	return activeRunner.Service() != nil
}

func (s *svc) wakeProgress() {
	if s.progress != nil {
		s.progress.Wake()
	}
}

func (s *svc) publishSubagentProgress(ctx context.Context, childID int64) {
	log := logger.Ctx(ctx).Named("daemon.progress")

	record, err := s.sessionStore.GetSession(ctx, childID)
	if err != nil {
		log.Warn("load_subagent_progress_session", zap.Int64("child", childID), zap.Error(err))

		return
	}

	if record.ParentID == 0 {
		return
	}

	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		log.Warn("load_subagent_progress_link", zap.Int64("child", childID), zap.Error(err))

		return
	}

	if link == nil {
		return
	}

	causalID := fmt.Sprintf("subagent:%d:%d:%s", childID, link.ActivationSeq, link.State)
	if link.Blocking && link.ParentID == record.RootID && !link.Terminal() {
		causalID, err = waitingProgressCausalID(s.collectWaitingProjections(ctx, record.RootID))
		if err != nil {
			log.Warn("build_subagent_progress_identity", zap.Int64("child", childID), zap.Error(err))

			return
		}
	}

	content, published, err := s.enqueueProgressChangeFor(ctx, record.RootID, causalID, true)
	if errors.Is(err, sessionstore.ErrOutputOwner) || errors.Is(err, sessionstore.ErrProgressSuperseded) {
		return
	}

	if err != nil {
		log.Warn("publish_subagent_progress", zap.Int64("child", childID), zap.Error(err))

		return
	}

	if published {
		s.publish(record.RootID, sessionevent.Notification{
			Type: sessionevent.NotifyMessage, Message: content,
		})
	}
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
		store, budgetSvc, daemon.HasActiveLoop, daemon.mainModelWorking, daemon.liveContextProjection,
		daemon.startBudgetPark, daemon.publish,
	)
}
