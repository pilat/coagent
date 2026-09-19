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
	"github.com/pilat/coagent/internal/subagent"
)

var errProgressUnavailable = errors.New("progress runtime unavailable")

var errWaitingSlotNotSuspended = errors.New("waiting slot requires a suspended root")

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
	// The durable status outranks the runner flag: a root parked on an
	// external call owns no runner service by the time its waiting card is
	// captured, but a concurrent capture can still observe the live loop
	// before finishRunner clears it — a suspended root is never "working".
	record, err := s.sessionStore.GetSession(context.Background(), rootID)
	if err != nil || record.Status == sessionstore.SessionStatusSuspended {
		return false
	}

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
	s.publishSubagentProgressWithIteration(ctx, childID, nil)
}

func (s *svc) publishSubagentIterationProgress(ctx context.Context, childID, iteration int64) {
	s.publishSubagentProgressWithIteration(ctx, childID, &iteration)
}

func (s *svc) publishSubagentProgressWithIteration(
	ctx context.Context,
	childID int64,
	checkpointIteration *int64,
) {
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

	if checkpointIteration != nil && link.Blocking {
		return
	}

	var causalID string
	if checkpointIteration != nil {
		causalID = fmt.Sprintf(
			"subagent:%d:%d:checkpoint:%d",
			childID,
			link.ActivationSeq,
			*checkpointIteration,
		)

		content, published, err := s.enqueueProgressChangeFor(ctx, record.RootID, causalID, true)
		s.settleSubagentProgress(log, record.RootID, childID, content, published, err)

		return
	}

	causalID, err = s.subagentStateCausalID(ctx, log, record, link, childID)
	if err != nil {
		return
	}

	content, published, err := s.enqueueProgressChangeFor(ctx, record.RootID, causalID, true)
	s.settleSubagentProgress(log, record.RootID, childID, content, published, err)
}

// subagentStateCausalID builds the state-transition causal identity. A
// blocking undelivered link publishes the root's whole waiting set instead.
func (s *svc) subagentStateCausalID(
	ctx context.Context,
	log *zap.Logger,
	record *sessionstore.SessionRecord,
	link *subagent.Link,
	childID int64,
) (string, error) {
	blockingRootLink := link.Blocking && link.ParentID == record.RootID
	if !blockingRootLink || link.Terminal() {
		return fmt.Sprintf("subagent:%d:%d:%s", childID, link.ActivationSeq, link.State), nil
	}

	root, err := s.sessionStore.GetSession(ctx, record.RootID)
	if err != nil {
		log.Warn("load_subagent_progress_root", zap.Int64("root", record.RootID), zap.Error(err))

		return "", fmt.Errorf("load progress root: %w", err)
	}

	// The waiting slot belongs to the suspension transition (publishWaiting):
	// a capture before suspension commits would freeze a stale card there.
	if root.Status != sessionstore.SessionStatusSuspended {
		return "", errWaitingSlotNotSuspended
	}

	return waitingProgressCausalID(s.collectWaitingProjections(ctx, record.RootID))
}

func (s *svc) settleSubagentProgress(
	log *zap.Logger,
	rootID, childID int64,
	content string,
	published bool,
	err error,
) {
	if errors.Is(err, sessionstore.ErrOutputOwner) || errors.Is(err, sessionstore.ErrProgressSuperseded) {
		return
	}

	if err != nil {
		log.Warn("publish_subagent_progress", zap.Int64("child", childID), zap.Error(err))

		return
	}

	if published {
		s.publish(rootID, sessionevent.Notification{
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
