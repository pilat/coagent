package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
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

type svc struct {
	runners    *registry[*runner]
	buildInput sessionbuild.BuildInput
	store      Store
	liveMu     sync.Mutex

	links        subagent.Store
	subagents    subagent.Transactions
	scheduleSvc  schedule.Service
	admit        *slots
	childQueue   *queue[queuedChild]
	pendingQueue *queue[queuedRunner]
	pubsub       sessionbus.Bus
	defaultModel string
	modelCatalog []subagent.ModelInfo
	modelEntries []config.ModelEntry
	mcpStore     mcpstore.Store
	applier      configapply.Service
	deferNotices *deferAnnouncements
	shuttingDown atomic.Bool
	recovery     *recovery

	progress      progressruntime.Service
	budgetCtx     context.Context //nolint:containedctx // Daemon lifetime context for joined park workers.
	budgetCancel  context.CancelFunc
	budgetWG      sync.WaitGroup
	budgetTimerMu sync.Mutex
	budgetTimers  map[int64]*budgetDeadline

	treeLocks sync.Map
	// Tree locks precede routeMu and childMu; never acquire a
	// tree lock while holding one of those narrower locks.
	// routeMu linearizes owner claims with replacement-session creation. The
	// daemon is single-instance, so this is the ownership CAS boundary.
	routeMu sync.Mutex
	// childMu guards publication routes only; runner lifecycle has its own registry.
	childMu    sync.Mutex
	childCache map[int64]bool
	ownerCache map[int64]string
	budgetSvc  budget.Service
	// processSvc owns live cancellation handles; processStore owns durability.
	processStore      backgroundprocess.Store
	processSvc        backgroundprocess.Service
	processRecoveryMu sync.Mutex
	processRecovery   chan struct{}
	workerCtx         context.Context //nolint:containedctx // Daemon lifetime context for joined workers.
	workerCancel      context.CancelFunc
	workerWG          sync.WaitGroup
	queueRetryMu      sync.Mutex
	queueRetryPending bool
}

func New(
	ctx context.Context,
	buildInput sessionbuild.BuildInput,
	store Store,

	links subagent.Store,
	subagents subagent.Transactions,
	budgetSvc budget.Service,
	processStore backgroundprocess.Store,
	progressSvc progressruntime.Service,
	pubsub sessionbus.Bus,
	scheduleSvc schedule.Service,
	cfg *config.Config,
	mcpStore mcpstore.Store,
	applier configapply.Service,
) Service {
	budgetCtx, budgetCancel := context.WithCancel(context.Background())
	workerCtx, workerCancel := context.WithCancel(context.Background())
	s := &svc{
		runners:      newRegistry[*runner](),
		buildInput:   buildInput,
		store:        store,
		processStore: processStore,
		mcpStore:     mcpStore,
		applier:      applier,

		links:        links,
		subagents:    subagents,
		budgetSvc:    budgetSvc,
		scheduleSvc:  scheduleSvc,
		admit:        &slots{perParent: make(map[int64]int)},
		childQueue:   newQueue[queuedChild](),
		pendingQueue: newQueue[queuedRunner](),
		recovery:     newRecovery(),
		pubsub:       pubsub,
		progress:     progressSvc,
		defaultModel: cfg.DefaultModel(),
		childCache:   make(map[int64]bool),
		ownerCache:   make(map[int64]string),
		deferNotices: newDeferAnnouncements(),
		workerCtx:    workerCtx,
		workerCancel: workerCancel,
		budgetCtx:    budgetCtx,
		budgetCancel: budgetCancel,
		budgetTimers: make(map[int64]*budgetDeadline),
	}

	if cfg.UnifiedConfig != nil {
		s.loadModelCatalog(cfg.UnifiedConfig.Models)
	}

	s.processSvc = s.newProcessService(ctx)
	s.buildInput.ProcessService = s.processSvc

	return s
}
