package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

var (
	_ Service                = (*svc)(nil)
	_ schedule.SessionSender = (*svc)(nil)
)

//nolint:interfacebloat // the daemon's whole operation surface; the Controller contract it backs is equally wide by design
type Service interface {
	Start(ctx context.Context) error
	Send(ctx context.Context, projectID int64, prompt, model string, attrs map[string]any) (int64, error)
	SendToSession(ctx context.Context, sessionID int64, prompt string) error
	SendToSessionResolved(ctx context.Context, sessionID int64, prompt string) (int64, error)
	Kill(ctx context.Context, sessionID int64) error
	Stop(ctx context.Context, sessionID, inputID int64) error
	Clear(ctx context.Context, sessionID int64) (int64, error)
	SetModel(ctx context.Context, sessionID int64, model, reasoningLevel string) error
	SetAttributes(ctx context.Context, sessionID int64, attrs map[string]any) error
	GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error)
	List(ctx context.Context) ([]*sessionstore.SessionRecord, error)
	HasActiveLoop(sessionID int64) bool
	CurrentProgress(ctx context.Context, rootID int64) (*controllerapi.ProgressData, error)
	RefreshProgress(ctx context.Context, rootID int64) error
	ReconcileOutputReadiness(ctx context.Context, outputID int64) error
	PubSub() sessionbus.Source
	NotifySession(sessionID int64, n sessionevent.Notification)
	Shutdown(timeout time.Duration)
	GetOrCreateProject(ctx context.Context, workDir string) (int64, error)
	GetOrCreateNamedProject(ctx context.Context, workDir, name string) (int64, error)
	GetOrCreateHiddenProject(ctx context.Context, workDir string) (int64, error)
	EnsureManagementRoot(
		ctx context.Context, projectID int64, owner string, topicID int64, name, workDir string,
	) (*sessionstore.SessionRecord, *sessionstore.OutputCommit, error)
	ListHiddenProjectDirs(ctx context.Context) ([]string, error)
	GetProjectWorkDir(ctx context.Context, projectID int64) (string, error)
	GetProjectName(ctx context.Context, projectID int64) (string, error)
	ListRecentProjects(ctx context.Context, root string) ([]controllerapi.RecentProjectInfo, error)
}

type svc struct {
	runners    *registry[*runner]
	buildInput sessionbuild.BuildInput
	store      Store
	liveMu     sync.Mutex

	links        subagent.Store
	subagents    subagent.Transactions
	scheduleSvc  schedule.Service
	admit        admission.Governor
	childQueue   *queue[queuedChild]
	pendingQueue *queue[queuedRunner]
	pubsub       sessionbus.Bus
	defaultModel string
	modelCatalog []subagent.ModelInfo
	modelEntries []config.ModelEntry
	// searchUnconfigured is the boot-time discoverability verdict: no
	// tools.search section and no native-capable model.
	searchUnconfigured bool
	mcpStore           mcpstore.Store
	applier            configapply.Service
	deferNotices       *deferAnnouncements
	shuttingDown       atomic.Bool
	recovery           *recovery

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
	s, processSvc := newSvc(
		ctx,
		buildInput, store,
		links, subagents, budgetSvc, processStore, progressSvc, pubsub,
		scheduleSvc, cfg.DefaultModel(),
	)
	s.mcpStore = mcpStore
	s.applier = applier
	s.searchUnconfigured = searchUnconfigured(cfg.UnifiedConfig)

	if cfg.UnifiedConfig != nil {
		s.loadModelCatalog(cfg.UnifiedConfig.Models)
	}

	if processSvc != nil {
		s.buildInput.ProcessService = processSvc
	}

	return s
}

func newSvc(
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
	defaultModel string,
) (*svc, backgroundprocess.Service) {
	budgetCtx, budgetCancel := context.WithCancel(context.Background())
	workerCtx, workerCancel := context.WithCancel(context.Background())
	s := &svc{
		runners:    newRegistry[*runner](),
		buildInput: buildInput,
		store:      store,

		links:        links,
		subagents:    subagents,
		budgetSvc:    budgetSvc,
		scheduleSvc:  scheduleSvc,
		admit:        admission.New(),
		childQueue:   newQueue[queuedChild](),
		pendingQueue: newQueue[queuedRunner](),
		recovery:     newRecovery(),
		pubsub:       pubsub,
		progress:     progressSvc,
		defaultModel: defaultModel,
		childCache:   make(map[int64]bool),
		ownerCache:   make(map[int64]string),
		deferNotices: newDeferAnnouncements(),
		workerCtx:    workerCtx,
		workerCancel: workerCancel,
		budgetCtx:    budgetCtx,
		budgetCancel: budgetCancel,
		budgetTimers: make(map[int64]*budgetDeadline),
	}

	// Background Bash processes: the ledger lives in the daemon database; the
	// coordinator routes terminal facts to owner or root sessions. The factory
	// shares one lifecycle service so all session stacks admit through it.

	var processSvc backgroundprocess.Service

	if processStore != nil {
		s.processStore = processStore
		processSvc = s.newProcessService(ctx)
		s.processSvc = processSvc
	}

	return s, processSvc
}
