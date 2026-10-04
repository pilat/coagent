package daemon

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/subagent"
)

var (
	_ Service                = (*svc)(nil)
	_ schedule.SessionSender = (*svc)(nil)
)

type Service interface {
	Start(ctx context.Context) error
	Shutdown(timeout time.Duration)
	Send(ctx context.Context, projectID int64, prompt, model string, attrs map[string]any) (int64, error)
	SendToSessionResolved(ctx context.Context, sessionID int64, prompt string) (int64, error)
	SetModel(ctx context.Context, sessionID int64, model, reasoningLevel string) error
	SetAttributes(ctx context.Context, sessionID int64, attrs map[string]any) error
	HasActiveLoop(sessionID int64) bool
	NotifySession(sessionID int64, n sessionevent.Notification)
}

type lifetime struct {
	mu      sync.Mutex
	ctx     context.Context //nolint:containedctx // Daemon lifetime for joined workers.
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	closing bool
}

type svc struct {
	store        Store
	links        subagent.Store
	schedules    schedule.Service
	budgets      budget.Service
	processStore backgroundprocess.Store
	processes    backgroundprocess.Service
	progress     progressruntime.Service
	applier      configapply.Service
	mcpStore     mcpstore.Store
	bus          sessionbus.Bus
	build        sessionbuild.BuildInput
	models       models
	life         *lifetime
	runners      *runnerSet
	trees        *treeLocks
	routes       *routes
	clock        *budgetClock
	liveMu       sync.Mutex
}

func New(
	ctx context.Context,
	build sessionbuild.BuildInput,
	store Store,

	links subagent.Store,
	budgetSvc budget.Service,
	processStore backgroundprocess.Store,
	progressSvc progressruntime.Service,
	pubsub sessionbus.Bus,
	scheduleSvc schedule.Service,
	cfg *config.Config,
	mcpStore mcpstore.Store,
	applier configapply.Service,
) Service {
	s := &svc{
		runners:      newRunnerSet(),
		build:        build,
		store:        store,
		processStore: processStore,
		mcpStore:     mcpStore,
		applier:      applier,

		links:     links,
		budgets:   budgetSvc,
		schedules: scheduleSvc,
		bus:       pubsub,
		progress:  progressSvc,
		trees:     &treeLocks{},
		routes:    &routes{child: make(map[int64]bool), owner: make(map[int64]string)},
		life:      newLifetime(),
		models:    newModels(cfg),
		clock:     newBudgetClock(),
	}

	s.processes = s.newProcessService(ctx)
	s.build.ProcessService = s.processes

	return s
}

func (s *svc) Shutdown(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	s.life.close()
	s.clock.close()
	runners := s.runners.closeAndSnapshot()
	done := make(chan struct{})

	go func() {
		for _, rs := range runners {
			rs.Cancel()
		}

		if _, err := s.processes.CancelAll(ctx, backgroundprocess.IntentDaemonShutdown); err != nil {
			logger.Ctx(ctx).Named("daemon.process").Warn("shutdown_cancel_failed", zap.Error(err))
		}

		_ = s.progress.Stop(ctx)

		for _, rs := range runners {
			<-rs.Done()
		}

		s.life.wait()

		if err := sessionbuild.CloseToolResources(s.build.Resources); err != nil {
			logger.Ctx(ctx).Named("manager.shutdown").Warn("close_tool_resources", zap.Error(err))
		}

		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		logger.Ctx(ctx).Named("manager.shutdown").Warn("shutdown_timeout", zap.Int("remaining_sessions", len(runners)))
	}
}

func newLifetime() *lifetime {
	ctx, cancel := context.WithCancel(context.Background())
	return &lifetime{ctx: ctx, cancel: cancel}
}

// Go runs fn on the lifetime context; a panic is logged under component.
func (l *lifetime) Go(component string, fn func(context.Context)) bool {
	if !l.enter() {
		return false
	}
	go func() {
		defer l.leave()
		defer func() {
			if v := recover(); v != nil {
				logger.Ctx(l.ctx).Named(component).Error("worker_panic", zap.Any("panic", v), zap.Stack("stack"))
			}
		}()

		fn(l.ctx)
	}()

	return true
}

// enter registers work Shutdown must wait for; false once Shutdown began.
func (l *lifetime) enter() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closing {
		return false
	}

	l.wg.Add(1)

	return true
}

func (l *lifetime) leave() { l.wg.Done() }

func (l *lifetime) closed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.closing
}

func (l *lifetime) close() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.closing = true
	l.cancel()
}

func (l *lifetime) wait() { l.wg.Wait() }

// whileOpen runs fn under the lifetime lock unless Shutdown began; close waits for it.
func (l *lifetime) whileOpen(fn func()) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closing {
		return false
	}

	fn()

	return true
}
