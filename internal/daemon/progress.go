package daemon

import (
	"context"
	"fmt"
	"sort"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/subagent"
)

type waitingProjection struct {
	wait sessionevent.WaitItem
	key  string
}

func (s *Service) liveContextProjection(ctx context.Context, rootID int64) (progress.Context, bool) {
	activeRunner, ok := s.runners.load(rootID)
	if !ok {
		return progress.Context{}, false
	}

	service := activeRunner.Service()
	if service == nil {
		return progress.Context{}, false
	}

	projection := service.ContextProjection(ctx)

	return progress.Context{
		Used: projection.Used, Max: projection.Max,
		Approximate: projection.Approximate, Available: projection.Available,
	}, true
}

func (s *Service) updateLive(ctx context.Context, sessionID int64) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()

	s.updateLiveLocked(ctx, sessionID)
}

func (s *Service) updateLiveLocked(ctx context.Context, sessionID int64) {
	live := progressruntime.Live{Active: s.HasActiveLoop(sessionID)}
	live.Context, _ = s.liveContextProjection(ctx, sessionID)
	s.progress.SetLive(sessionID, live)
}

func (s *Service) publishWaiting(ctx context.Context, sessionID int64) {
	projections := s.collectWaitingProjections(ctx, sessionID)
	if len(projections) == 0 {
		return
	}

	waits := make([]sessionevent.WaitItem, len(projections))
	for i, projection := range projections {
		waits[i] = projection.wait
	}

	s.publish(sessionID, sessionevent.Notification{
		Type: sessionevent.NotifyWaiting, Message: sessionevent.FormatWaiting(waits), Waiting: waits,
	})
}

func (s *Service) collectWaitingProjections(ctx context.Context, sessionID int64) []waitingProjection {
	projections := make([]waitingProjection, 0)
	log := logger.Ctx(ctx).Named("daemon.waiting")

	sleeps, err := s.schedules.PendingSleeps(ctx, sessionID)
	if err != nil {
		log.Warn("list_pending_sleeps", zap.Error(err))
	} else {
		for _, sleep := range sleeps {
			wakeAt := sleep.WakeAt
			projections = append(projections, waitingProjection{
				wait: sessionevent.WaitItem{Kind: sessionevent.WaitSleep, WakeAt: &wakeAt},
				key:  "1:" + sleep.CallID,
			})
		}
	}

	links, err := s.links.ListPendingChildLinks(ctx, sessionID)
	if err != nil {
		log.Warn("list_subagents", zap.Error(err))
	} else {
		for _, link := range links {
			if link.Blocking && !link.Terminal() && link.State != subagent.StateStopped {
				projections = append(projections, waitingProjection{
					wait: sessionevent.WaitItem{Kind: sessionevent.WaitSubagent, ChildID: link.ChildID},
					key:  fmt.Sprintf("0:%020d:%020d", link.ChildID, link.ActivationSeq),
				})
			}
		}
	}

	sort.Slice(projections, func(i, j int) bool { return projections[i].key < projections[j].key })

	return projections
}
