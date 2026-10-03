package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionlifecycle"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
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

const (
	stopCommand    = "/stop"
	clearCommand   = "/clear"
	killCommand    = "/kill"
	compactCommand = "/compact"
)

var (
	_ Service                = (*svc)(nil)
	_ schedule.SessionSender = (*svc)(nil)

	errDaemonShuttingDown = sessionlifecycle.ErrShuttingDown
)

type svc struct {
	runners         sessionlifecycle.Registry[runner]
	buildInput      sessionbuild.BuildInput
	store           Store
	sessionStore    sessionstore.OrchestrationStore
	inboxStore      sessionstore.InboxStore
	activationStore sessionstore.ActivationStore
	runtimeStore    sessionstore.AgentRuntimeStore
	managerOutputs  sessionstore.ManagerOutputStore
	managerRoots    sessionstore.ManagerRootTransactions
	lifecycleStore  sessionstore.SessionLifecycleStore
	modelInputs     sessionstore.Store
	links           subagent.Store
	subagents       subagent.Transactions
	scheduleSvc     schedule.Service
	admit           admission.Governor
	childQueue      sessionlifecycle.Queue[queuedChild]
	pendingQueue    sessionlifecycle.Queue[queuedRunner]
	pubsub          sessionbus.Bus
	defaultModel    string
	modelCatalog    []subagent.ModelInfo
	modelEntries    []config.ModelEntry
	// searchUnconfigured is the boot-time discoverability verdict: no
	// tools.search section and no native-capable model.
	searchUnconfigured bool
	mcpStore           mcpstore.Store
	applier            configapply.Service
	deferNotices       *deferAnnouncements
	shuttingDown       atomic.Bool
	recovery           sessionlifecycle.Recovery
	stopper            sessionlifecycle.Stopper
	completions        sessionlifecycle.Completions
	launcher           sessionlifecycle.Launcher
	progress           progressruntime.Service
	budgetCtx          context.Context //nolint:containedctx // Daemon lifetime context for joined park workers.
	budgetCancel       context.CancelFunc
	budgetWG           sync.WaitGroup
	treeStore          sessionstore.OrchestrationStore
	treeLocks          sync.Map
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

// OutputStore exposes the narrow manager-delivery ledger without widening the
// daemon's general Service interface used by controller fakes.
func (s *svc) OutputStore() sessionstore.ManagerOutputStore {
	return s.managerOutputs
}

// queuedChild is a background child that could not be admitted immediately and
// waits (in arrival order) for a slot to free. Durability comes from its
// already-persisted subagent_links row (state 'spawned', inserted by Spawn before
// admission) — the restart sweep re-runs it on crash; this slice is only the
// in-memory ordering cache.
type queuedChild struct {
	sessionID int64
	parentID  int64
	workDir   string
	projectID int64
}

type queuedRunner struct {
	sessionID int64
	workDir   string
	projectID int64
}

func New(
	ctx context.Context,
	buildInput sessionbuild.BuildInput,
	store Store,
	sessionStore sessionstore.OrchestrationStore,
	inboxStore sessionstore.Store,
	runtimeStore sessionstore.AgentRuntimeStore,
	managerOutputs sessionstore.ManagerOutputStore,
	managerRoots sessionstore.ManagerRootTransactions,
	lifecycleStore sessionstore.SessionLifecycleStore,
	modelInputs sessionstore.Store,
	links subagent.Store,
	subagents subagent.Transactions,
	budgetSvc budget.Service,
	progressStore progressruntime.Store,
	scheduleSvc schedule.Service,
	cfg *config.Config,
	mcpStore mcpstore.Store,
	applier configapply.Service,
) Service {
	s, processSvc := newSvc(
		ctx,
		buildInput, store, sessionStore, inboxStore, runtimeStore,
		managerOutputs, managerRoots, lifecycleStore, modelInputs,
		links, subagents, budgetSvc, progressStore,
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
	sessionStore sessionstore.OrchestrationStore,
	inboxStore sessionstore.Store,
	runtimeStore sessionstore.AgentRuntimeStore,
	managerOutputs sessionstore.ManagerOutputStore,
	managerRoots sessionstore.ManagerRootTransactions,
	lifecycleStore sessionstore.SessionLifecycleStore,
	modelInputs sessionstore.Store,
	links subagent.Store,
	subagents subagent.Transactions,
	budgetSvc budget.Service,
	progressStore progressruntime.Store,
	scheduleSvc schedule.Service,
	defaultModel string,
) (*svc, backgroundprocess.Service) {
	budgetCtx, budgetCancel := context.WithCancel(context.Background())
	workerCtx, workerCancel := context.WithCancel(context.Background())
	s := &svc{
		runners:         sessionlifecycle.NewRegistry[runner](),
		buildInput:      buildInput,
		store:           store,
		sessionStore:    sessionStore,
		treeStore:       sessionStore,
		inboxStore:      inboxStore,
		activationStore: inboxStore,
		runtimeStore:    runtimeStore,
		managerOutputs:  managerOutputs,
		managerRoots:    managerRoots,
		lifecycleStore:  lifecycleStore,
		modelInputs:     modelInputs,
		links:           links,
		subagents:       subagents,
		budgetSvc:       budgetSvc,
		scheduleSvc:     scheduleSvc,
		admit:           admission.New(),
		childQueue:      sessionlifecycle.NewQueue[queuedChild](),
		pendingQueue:    sessionlifecycle.NewQueue[queuedRunner](),
		recovery:        sessionlifecycle.NewRecovery(),
		stopper: sessionlifecycle.NewStopper(
			sessionStore, lifecycleStore, managerOutputs, links,
		),
		pubsub:       sessionbus.New(),
		defaultModel: defaultModel,
		childCache:   make(map[int64]bool),
		ownerCache:   make(map[int64]string),
		deferNotices: newDeferAnnouncements(),
		workerCtx:    workerCtx,
		workerCancel: workerCancel,
		budgetCtx:    budgetCtx,
		budgetCancel: budgetCancel,
	}
	s.progress = newProgressRuntime(progressStore, budgetSvc, s)
	s.completions = s.newCompletionCoordinator()
	s.launcher = sessionlifecycle.NewLauncher(
		sessionStore, links, s.admit, s.runners,
		s.ensureRunnerStartable, s.enqueueCapacityBlockedChild, s.runSession,
	)

	// Background Bash processes: the ledger lives in the daemon database; the
	// coordinator routes terminal facts to owner or root sessions. The factory
	// shares one lifecycle service so all session stacks admit through it.

	var processSvc backgroundprocess.Service

	if rawStore, ok := store.(interface{ DB() *sql.DB }); ok {
		if db := rawStore.DB(); db != nil {
			s.processStore = backgroundprocess.NewStore(db, inboxStore)
			processSvc = s.newProcessService(ctx)
			s.processSvc = processSvc
		}
	}

	return s, processSvc
}

func (s *svc) PubSub() sessionbus.Source {
	return s.pubsub
}

func (s *svc) NotifySession(sessionID int64, n sessionevent.Notification) {
	s.publish(sessionID, n)
}

func (s *svc) Send(ctx context.Context, projectID int64, prompt, model string, attrs map[string]any) (int64, error) {
	return s.send(ctx, projectID, prompt, model, attrs)
}

func (s *svc) SendToSession(ctx context.Context, sessionID int64, prompt string) error {
	input, err := s.enqueueUserSessionInput(ctx, sessionID, prompt)
	if err != nil {
		if errors.Is(err, sessionstore.ErrSessionNotAcceptingInput) {
			if record, getErr := s.sessionStore.GetSession(ctx, sessionID); getErr == nil && record.KilledAt != nil {
				return fmt.Errorf("session %d is killed", sessionID)
			}
		}

		// The park drain won the arbitration CAS; the retry is the user's next
		// message once the parked root accepts input again.
		if errors.Is(err, budget.ErrConflict) {
			return fmt.Errorf(
				"session %d is parking after a budget checkpoint — send the message again once it stops",
				sessionID,
			)
		}

		return fmt.Errorf("persist session input: %w", err)
	}

	if handled, err := s.handleGenericCommand(ctx, input); handled || err != nil {
		return err
	}

	_, ok := s.runners.Load(sessionID)
	if ok {
		return nil
	}

	rec, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session %d not found", sessionID)
	}

	if rec.KilledAt != nil {
		return fmt.Errorf("session %d is killed", sessionID)
	}

	if rec.Status == sessionstore.SessionStatusStopping {
		return fmt.Errorf("session %d is stopping", sessionID)
	}

	parked, err := s.prepareStoppedSessionInput(ctx, rec, prompt)
	if err != nil {
		return err
	}

	if parked {
		return nil
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	if err != nil {
		return fmt.Errorf("resolve project %d: %w", rec.ProjectID, err)
	}

	if _, ok = s.runners.Load(sessionID); ok {
		return nil
	}

	if err := s.ensureRunner(ctx, sessionID, workDir, rec.ProjectID); err != nil {
		if errors.Is(err, admission.ErrNoCapacity) {
			s.enqueuePendingRunner(sessionID, workDir, rec.ProjectID)
			return nil
		}

		return err
	}

	return nil
}

func (s *svc) SendToSessionResolved(ctx context.Context, sessionID int64, prompt string) (int64, error) {
	record, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("load session for replacement resolution: %w", err)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)

	resolved, err := s.managerRoots.ResolveReplacement(ctx, sessionID, owner)
	if err != nil {
		return 0, fmt.Errorf("resolve replacement session: %w", err)
	}

	sessionID = resolved

	if err := s.SendToSession(ctx, sessionID, prompt); err != nil {
		return 0, err
	}

	return sessionID, nil
}

func isReadOnlyBoundaryCommand(content string) bool {
	content = strings.TrimSpace(content)

	return content == "/status" || content == "/help" || content == "/schedules" ||
		content == compactCommand || strings.HasPrefix(content, compactCommand+" ")
}

func isExactControlCommand(content string) bool {
	content = strings.TrimSpace(content)

	return isReadOnlyBoundaryCommand(content) || content == stopCommand || content == clearCommand ||
		content == killCommand
}

//nolint:funcorder // Command dispatch remains beside durable input admission and lifecycle fencing.
func (s *svc) handleGenericCommand(ctx context.Context, input *sessionstore.InboxInput) (bool, error) {
	if input.Source != sessionstore.InputSourceUser {
		return false, nil
	}

	if strings.TrimSpace(input.RawContent) == "/status" {
		return true, s.handleStatusInput(ctx, input)
	}

	command := strings.TrimSpace(input.RawContent)
	if command != stopCommand && command != clearCommand && command != killCommand {
		return false, nil
	}

	unlock, err := s.lockSessionTree(ctx, input.SessionID)
	if err != nil {
		return true, err
	}
	defer unlock()

	switch command {
	case stopCommand:
		record, err := s.sessionStore.GetSession(ctx, input.SessionID)
		if err != nil {
			return true, fmt.Errorf("load stop session: %w", err)
		}

		if record.Status == sessionstore.SessionStatusStopped {
			return true, s.handleStoppedStop(ctx, input)
		}

		if err := s.handleLifecycleInput(ctx, input, "⏳ Stopping…"); err != nil {
			return true, err
		}

		return true, s.stopLocked(ctx, input.SessionID, input.ID)
	case clearCommand:
		if _, err := s.clearLocked(ctx, input.SessionID, input.ID); err != nil {
			return true, err
		}

		return true, nil
	case killCommand:
		if err := s.handleLifecycleInput(ctx, input, "Stopping session..."); err != nil {
			return true, err
		}

		return true, s.killLocked(ctx, input.SessionID)
	default:
		return false, nil
	}
}

//nolint:funcorder // Immediate status dispatch belongs beside the generic command boundary.
func (s *svc) handleStatusInput(ctx context.Context, input *sessionstore.InboxInput) error {
	current, err := s.CurrentProgress(ctx, input.SessionID)
	if err != nil {
		return err
	}
	_, err = s.runtimeStore.Commit(ctx, sessionstore.Commit{
		SessionID: input.SessionID,
		Accept: []sessionstore.Accept{
			{InputID: input.ID, State: sessionstore.InputStateHandled, Reason: "status command", LinkRef: -1},
		},
		Outputs: []sessionstore.Output{
			{Type: sessionstore.OutputMessagePersistent, Content: current.Rendered, MessageRef: -1},
		},
	})
	if errors.Is(err, sessionstore.ErrInputResolved) {
		return nil
	}
	if err != nil {
		return err
	}
	s.publish(input.SessionID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: current.Rendered})
	if !s.HasActiveLoop(input.SessionID) {
		s.publish(
			input.SessionID,
			sessionevent.Notification{Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle},
		)
	}
	return nil
}

//nolint:funcorder // The idempotent stop result is part of the same command dispatcher.
func (s *svc) handleStoppedStop(ctx context.Context, input *sessionstore.InboxInput) error {
	_, err := s.runtimeStore.Commit(ctx, sessionstore.Commit{
		SessionID: input.SessionID,
		Accept: []sessionstore.Accept{
			{InputID: input.ID, State: sessionstore.InputStateHandled, Reason: "stop command", LinkRef: -1},
		},
		Outputs: []sessionstore.Output{
			{
				Type:       sessionstore.OutputMessagePersistent,
				Content:    "Session already stopped.",
				Key:        fmt.Sprintf("input:%d:stop:already_stopped", input.ID),
				MessageRef: -1,
			},
		},
	})
	return err
}

//nolint:funcorder // Lifecycle input must stay with the generic dispatcher that invokes it.
func (s *svc) handleLifecycleInput(ctx context.Context, input *sessionstore.InboxInput, content string) error {
	command := strings.TrimPrefix(strings.TrimSpace(input.RawContent), "/")

	if _, owned := input.Attributes[controllerapi.SessionAttributeManagerID].(string); !owned {
		if _, err := s.runtimeStore.Commit(
			ctx,
			sessionstore.Commit{
				SessionID: input.SessionID,
				Accept: []sessionstore.Accept{
					{InputID: input.ID, State: sessionstore.InputStateHandled, Reason: command, LinkRef: -1},
				},
			},
		); err != nil {
			return fmt.Errorf("handle lifecycle input: %w", err)
		}

		return nil
	}

	if _, err := s.lifecycleStore.BeginLifecycleInput(ctx, input.ID, command, content); err != nil {
		return fmt.Errorf("start lifecycle input: %w", err)
	}

	return nil
}

//nolint:funcorder // Daemon producers share this helper with the adjacent command boundary.
func (s *svc) enqueuePersistentOutput(ctx context.Context, sessionID int64, content string) error {
	outputs := s.OutputStore()
	if outputs == nil {
		return nil
	}

	if _, err := outputs.EnqueueOutput(ctx, sessionstore.OutputDraft{
		SessionID: sessionID, Type: sessionstore.OutputMessagePersistent, Content: content,
	}); err != nil {
		return fmt.Errorf("enqueue persistent output: %w", err)
	}

	return nil
}

func (s *svc) GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	rec, err := s.sessionStore.GetSession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load session record: %w", err)
	}

	return rec, nil
}

func (s *svc) List(ctx context.Context) ([]*sessionstore.SessionRecord, error) {
	recs, err := s.sessionStore.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	return recs, nil
}

func (s *svc) HasActiveLoop(sessionID int64) bool {
	_, ok := s.runners.Load(sessionID)

	return ok
}

func (s *svc) Kill(ctx context.Context, sessionID int64) error {
	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return err
	}
	defer unlock()

	return s.killLocked(ctx, sessionID)
}

//nolint:funcorder // Public lifecycle methods delegate into the shared tree lock.
func (s *svc) killLocked(ctx context.Context, sessionID int64) error {
	rs, ok := s.runners.Load(sessionID)

	if ok {
		s.publish(
			sessionID,
			sessionevent.Notification{
				Type:    sessionevent.NotifyMessage,
				Message: "Stopping session...",
			},
		)

		rs.Stop()
	}

	rec, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session %d not found", sessionID)
	}

	if rec.KilledAt != nil {
		return s.retireTreeToolResources(ctx, sessionID)
	}

	if rec.ParentID == 0 {
		if err := s.releaseArmedBudget(ctx, sessionID, "killed"); err != nil {
			return err
		}
	}

	// Cleanup must complete even if the caller disconnects mid-Kill — detach
	// from request-scoped cancellation while keeping logger values.
	cleanupCtx := context.WithoutCancel(ctx)

	cancelledProcesses := 0
	if s.processSvc != nil {
		cancelledProcesses, err = s.cancelSessionSubtreeProcesses(
			cleanupCtx, sessionID, backgroundprocess.IntentSessionKilled,
		)
		if err != nil {
			return fmt.Errorf("cancel background processes: %w", err)
		}
	}

	if _, err := s.lifecycleStore.MarkSessionKilledWithOutput(
		cleanupCtx, sessionID, cancelledProcesses,
	); err != nil {
		return fmt.Errorf("mark session killed with output: %w", err)
	}

	if err := s.retireTreeToolResources(cleanupCtx, sessionID); err != nil {
		return err
	}

	s.removeSchedules(cleanupCtx, sessionID)

	// Cascade-kill every non-terminal descendant (blocking and background): this is
	// a deliberate tree teardown, so background work that would outlive it and
	// report to nobody is stopped too. Completed-but-undelivered children keep their
	// result (see cascadeKillChildren).
	s.cascadeKillChildrenForKilledTree(
		cleanupCtx, sessionID, 0, time.Now().Add(cascadeRetryBudget),
	)

	if ownerlessSession(rec) {
		s.publish(sessionID, sessionevent.Notification{
			Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle, Reason: "killed",
		})
	}

	return nil
}

// Stop parks a session tree without destroying it. Every active descendant is
// stopped, one-shot waits and pending external calls receive an explicit stopped
// result, and accepted-but-unconsumed input is cancelled. Recurring schedules
// remain installed. A later root message resumes only the root; a stopped child
// requires an explicit send_to_subagent follow-up.
//
// An explicit manager-owned /stop (inputID > 0) leaves its root in `stopping`
// after cleanup and commits the durable terminal output in one transaction with
// the budget release and the final stopped status. A failure before that
// commit leaves the root stopping and publishes no success.
func (s *svc) Stop(ctx context.Context, sessionID, inputID int64) error {
	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return err
	}
	defer unlock()

	return s.stopLocked(ctx, sessionID, inputID)
}

//nolint:funcorder // Public lifecycle methods delegate into the shared tree lock.
func (s *svc) stopLocked(ctx context.Context, sessionID, inputID int64) error {
	record, getErr := s.sessionStore.GetSession(ctx, sessionID)
	if getErr != nil {
		// Fail closed: an unread session must not be classified as ownerless,
		// so the idle publication is skipped instead of faked.
		logger.Ctx(ctx).Warn("stop_session_lookup_failed",
			zap.Int64("session_id", sessionID), zap.Error(getErr))

		record = nil
	}

	explicit := inputID > 0 && record != nil && !ownerlessSession(record)

	if !explicit {
		s.publish(sessionID, sessionevent.Notification{
			Type:    sessionevent.NotifyMessage,
			Message: "⏹ Stopping...",
		})
	}

	cancelledProcesses := 0
	if err := s.stopTreeCleanup(ctx, sessionID, stopTreeOptions{
		keepRootStopping: explicit, cancelledProcesses: &cancelledProcesses,
	}); err != nil {
		return err
	}

	if explicit {
		return s.completeExplicitStop(ctx, sessionID, inputID, cancelledProcesses)
	}

	if err := s.releaseArmedBudget(ctx, sessionID, "stopped"); err != nil {
		return err
	}

	if err := s.convergeOrphanedStopStart(ctx, sessionID, inputID, cancelledProcesses); err != nil {
		return err
	}

	if record != nil && ownerlessSession(record) {
		s.publish(sessionID, sessionevent.Notification{
			Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle, Reason: "stopped",
		})
	}

	return nil
}

// convergeOrphanedStopStart finishes the terminal fact when the stop fence
// committed a start row before the ownership check could classify the stop.
// Without it a replaceable "Stopping…" receipt would stay dangling with no
// recovery path, because startup only converges roots still in `stopping`.
//
//nolint:funcorder // completes the stop transition documented above.
func (s *svc) convergeOrphanedStopStart(
	ctx context.Context,
	sessionID, inputID int64,
	cancelledProcesses int,
) error {
	if inputID <= 0 {
		return nil
	}

	record, recordErr := s.sessionStore.GetSession(ctx, sessionID)
	// Ownerless stops have no start row to converge; an unread session stays a
	// startup-recovery case rather than a terminal fact published blind.
	if recordErr == nil && !ownerlessSession(record) {
		return s.completeExplicitStop(ctx, sessionID, inputID, cancelledProcesses)
	}

	return nil
}

// completeExplicitStop commits the terminal stop fact and then issues a
// non-authoritative delivery wake, so the manager need not wait for its idle
// rescan. The outbox remains the source of truth either way.
//
//nolint:funcorder // belongs beside the public Stop transition it completes.
func (s *svc) completeExplicitStop(
	ctx context.Context,
	rootID, inputID int64,
	cancelledProcesses int,
) error {
	return s.stopper.CompleteExplicit( //nolint:wrapcheck // Stopper owns terminal context.
		ctx, rootID, inputID, cancelledProcesses,
	)
}

// stopTreeCleanup durably parks a tree without publishing user-command events.
// Startup recovery and budget parking use it to finish an interrupted stop
// without replaying UI. With keepRootStopping the explicit root stays in its
// stopping fence: the caller owns the single terminal transaction that moves it
// to `stopped` together with the visible completion output.
type stopTreeOptions struct {
	keepRootStopping            bool
	preserveBackgroundProcesses bool
	cancelledProcesses          *int
}

//nolint:funcorder,wsl_v5 // The second stop phase belongs beside the public Stop transition.
func (s *svc) stopTreeCleanup(ctx context.Context, sessionID int64, options stopTreeOptions) error {
	cleanupCtx := context.WithoutCancel(ctx)

	liveSessionIDs, err := s.liveTreeRunnerIDs(cleanupCtx, sessionID)
	if err != nil {
		return err
	}
	plan, err := s.stopper.Begin(cleanupCtx, sessionID, liveSessionIDs)
	if err != nil {
		return fmt.Errorf("begin stop tree: %w", err)
	}

	ids := plan.SessionIDs()

	s.removeQueuedSessions(ids)

	runners := make([]runner, 0, len(ids))
	for _, id := range ids {
		rs, _ := s.runners.Load(id)

		if rs != nil {
			runners = append(runners, rs)
		}
	}

	// Signal the entire tree before waiting for any one runner: a foreground
	// parent can otherwise keep a child alive while stop is waiting on it.
	for _, rs := range runners {
		rs.Cancel()
	}
	// Cancel every background Bash process owned by the tree, including
	// processes started by already-terminal subagents. The shared lifecycle
	// service signals the in-memory handles, joins terminalization, and
	// suppresses the individual wake events before the stop fence completes.
	if err := s.stopTreeBackgroundProcesses(cleanupCtx, sessionID, options); err != nil {
		return err
	}

	for _, rs := range runners {
		<-rs.Done()
	}
	if err := s.retireTreeToolResources(cleanupCtx, sessionID); err != nil {
		return err
	}

	if err := s.stopper.CancelInputs(cleanupCtx, plan); err != nil {
		return fmt.Errorf("cancel stopped inputs: %w", err)
	}

	if err := s.settleStoppedTree(cleanupCtx, ids); err != nil {
		return err
	}

	if err := s.stopper.Finish(cleanupCtx, plan, options.keepRootStopping); err != nil {
		return fmt.Errorf("finish stop tree: %w", err)
	}

	return nil
}

//nolint:funcorder // Background cancellation is a phase of the adjacent stop transition.
func (s *svc) stopTreeBackgroundProcesses(ctx context.Context, sessionID int64, options stopTreeOptions) error {
	if s.processSvc == nil || options.preserveBackgroundProcesses {
		return nil
	}

	cancelled, err := s.cancelSessionSubtreeProcesses(ctx, sessionID, backgroundprocess.IntentSessionStopped)
	if err != nil {
		return fmt.Errorf("cancel background processes: %w", err)
	}

	if options.cancelledProcesses != nil {
		*options.cancelledProcesses = cancelled
	}

	return nil
}

// settleStoppedTree closes every outstanding tool_use once all writers have
// joined. That is what makes a stopped session resumable without replaying a
// sleep/config/task call that no longer exists.
//
//nolint:funcorder // Stop-tree helpers stay beside the stop they serve.
func (s *svc) settleStoppedTree(ctx context.Context, ids []int64) error {
	for _, id := range ids {
		if err := s.settleStoppedCalls(ctx, id); err != nil {
			return err
		}

		// The settlement just answered every pending call, so a pending grant
		// can never be spent anymore; expire it store-only or its row sits
		// pending until the next wake burns a model turn on the receipt.
		if s.scheduleSvc == nil {
			continue
		}

		if _, err := s.scheduleSvc.CancelPendingSleeps(ctx, id); err != nil {
			return fmt.Errorf("cancel one-shot waits for session %d: %w", id, err)
		}
	}

	return nil
}

//nolint:funcorder,wsl_v5 // Runner discovery must immediately precede stop planning.
func (s *svc) liveTreeRunnerIDs(ctx context.Context, rootID int64) ([]int64, error) {
	records, err := s.sessionStore.ListAllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list live tree runners: %w", err)
	}

	var ids []int64
	for _, record := range records {
		if record.ID != rootID && record.RootID != rootID {
			continue
		}
		if _, ok := s.runners.Load(record.ID); ok {
			ids = append(ids, record.ID)
		}
	}

	return ids, nil
}

func (s *svc) Clear(ctx context.Context, sessionID int64) (int64, error) {
	return s.clear(ctx, sessionID, 0)
}

//nolint:funcorder // Clear's command variant shares one replacement transaction with Clear.
func (s *svc) clear(ctx context.Context, sessionID, inputID int64) (int64, error) {
	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	defer unlock()

	return s.clearLocked(ctx, sessionID, inputID)
}

//nolint:funcorder // Public lifecycle methods delegate into the shared tree lock.
func (s *svc) clearLocked(ctx context.Context, sessionID, inputID int64) (int64, error) {
	log := logger.Ctx(ctx).Named("manager.clear")

	s.routeMu.Lock()
	defer s.routeMu.Unlock()

	rec, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("session %d not found", sessionID)
	}

	if rec.KilledAt != nil {
		return 0, fmt.Errorf("session %d is already killed", sessionID)
	}

	workDir, _ := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	projectName, _ := s.store.GetProjectName(ctx, rec.ProjectID)
	owner, _ := rec.Attributes[controllerapi.SessionAttributeManagerID].(string)
	var newRec *sessionstore.SessionRecord

	//nolint:nestif // Owner-aware replacement is the one boundary that preserves a manager surface.
	if owner != "" {
		if inputID > 0 {
			newRec, _, err = s.managerRoots.ReplaceManagerRootForInput(ctx, sessionID, inputID, projectName, workDir)
		} else {
			newRec, _, err = s.managerRoots.ReplaceManagerRoot(ctx, sessionID, projectName, workDir)
		}

		if err != nil {
			return 0, fmt.Errorf("replace manager session: %w", err)
		}
	} else {
		newRec, err = s.sessionStore.CreateReplacementSession(ctx, sessionID)
		if err != nil {
			return 0, fmt.Errorf("create replacement session: %w", err)
		}
	}

	name := fmt.Sprintf("%s - %d", projectName, newRec.ID)
	s.publish(sessionID, sessionevent.Notification{
		Type:         sessionevent.NotifySessionCleared,
		OldSessionID: sessionID,
		NewSessionID: newRec.ID,
		Name:         name,
		WorkDir:      workDir,
		Attributes:   rec.Attributes,
	})

	if err := s.killLocked(ctx, sessionID); err != nil {
		log.Warn("clear_kill_old_session_failed", zap.Int64("session_id", sessionID), zap.Error(err))
	}

	return newRec.ID, nil
}

// Model publication and construction share the tree fence so the record and
// the next activation cannot disagree about which client to adopt.
func (s *svc) SetModel(ctx context.Context, sessionID int64, model, reasoningLevel string) error {
	if err := s.checkModelConfigured(model); err != nil {
		return err
	}

	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return err
	}
	defer unlock()
	record, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session for model switch: %w", err)
	}
	if s.budgetSvc != nil {
		budgetRecord, budgetErr := s.budgetSvc.Get(ctx, sessionRootID(record))
		if budgetErr == nil && budgetRecord.State == budget.Armed &&
			budgetRecord.CostLimitUSD != nil && !s.modelHasPricing(model) {
			return errors.New("cannot switch an armed budget tree to a model without catalog pricing")
		}

		if budgetErr != nil && !errors.Is(budgetErr, budget.ErrNotFound) {
			return fmt.Errorf("load budget for model switch: %w", budgetErr)
		}
	}

	client, section, err := sessionbuild.BuildClient(s.buildInput.Config, model, reasoningLevel)
	if err != nil {
		return fmt.Errorf("construct model %s: %w", model, err)
	}
	level := client.GetReasoningLevel()
	if err := s.sessionStore.UpdateSessionModel(ctx, sessionID, model, level); err != nil {
		_ = client.Close()
		return fmt.Errorf("update session model: %w", err)
	}
	rs, ok := s.runners.Load(sessionID)
	if ok {
		if sess := rs.Service(); sess != nil {
			sess.SwitchModel(client, section)
			return nil
		}
	}
	_ = client.Close()

	return nil
}

func (s *svc) SetAttributes(ctx context.Context, sessionID int64, attrs map[string]any) error {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()

	rec, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session before setting attributes: %w", err)
	}

	if rec == nil {
		return fmt.Errorf("session %d not found", sessionID)
	}

	attrs = maps.Clone(attrs)
	existingOwner, _ := rec.Attributes[controllerapi.SessionAttributeManagerID].(string)

	requestedOwner, _ := attrs[controllerapi.SessionAttributeManagerID].(string)
	claimingOwner := existingOwner == "" && requestedOwner != ""

	if claimingOwner && (rec.Status == sessionstore.SessionStatusTerminating || rec.KilledAt != nil) {
		return fmt.Errorf("session %d is closing and cannot acquire a manager owner", sessionID)
	}

	if existingOwner != "" && requestedOwner != "" && existingOwner != requestedOwner {
		return fmt.Errorf("session %d belongs to manager %q", sessionID, existingOwner)
	}

	if existingOwner != "" {
		if attrs == nil {
			attrs = make(map[string]any)
		}

		attrs[controllerapi.SessionAttributeManagerID] = existingOwner
		requestedOwner = existingOwner
	}

	if err := s.sessionStore.SetAttributes(ctx, sessionID, attrs); err != nil {
		return fmt.Errorf("set session attributes: %w", err)
	}

	s.childMu.Lock()
	s.ownerCache[sessionID] = requestedOwner
	s.childMu.Unlock()

	return nil
}

func (s *svc) Shutdown(timeout time.Duration) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	s.shuttingDown.Store(true)
	s.queueRetryMu.Lock()
	if s.workerCancel != nil {
		s.workerCancel()
	}
	s.queueRetryMu.Unlock()

	if s.budgetCancel != nil {
		s.budgetCancel()
	}

	recoveryDone := s.stopRecovery()
	processRecoveryDone := s.currentProcessRecovery()

	runners := s.runners.CloseAndSnapshot()

	done := make(chan struct{})

	go func() {
		for _, rs := range runners {
			rs.Cancel()
		}

		// Controlled shutdown cancels and joins every owned process group
		// through the shared lifecycle service; their completions stay owed
		// as interrupted for the next startup.
		if s.processSvc != nil {
			if _, err := s.processSvc.CancelAll(shutdownCtx, backgroundprocess.IntentDaemonShutdown); err != nil {
				logger.Ctx(shutdownCtx).Named("daemon.process").Warn("shutdown_cancel_failed", zap.Error(err))
			}
		}

		if s.progress != nil {
			_ = s.progress.Stop(shutdownCtx)
		}

		for _, rs := range runners {
			<-rs.Done()
		}

		if recoveryDone != nil {
			<-recoveryDone
		}

		if processRecoveryDone != nil {
			<-processRecoveryDone
		}

		s.budgetWG.Wait()
		s.workerWG.Wait()

		if err := sessionbuild.CloseToolResources(s.buildInput.Resources); err != nil {
			logger.Named("manager.shutdown").Warn("close_tool_resources", zap.Error(err))
		}

		close(done)
	}()

	select {
	case <-done:
	case <-shutdownCtx.Done():
		logger.Named("manager.shutdown").Warn("shutdown_timeout", zap.Int("remaining_sessions", len(runners)))
	}
}

func (s *svc) GetOrCreateProject(ctx context.Context, workDir string) (int64, error) {
	id, err := s.store.GetOrCreateProject(ctx, workDir)
	if err != nil {
		return 0, fmt.Errorf("resolve project: %w", err)
	}

	return id, nil
}

func (s *svc) GetOrCreateNamedProject(ctx context.Context, workDir, name string) (int64, error) {
	id, err := s.store.GetOrCreateNamedProject(ctx, workDir, name)
	if err != nil {
		return 0, fmt.Errorf("resolve named project: %w", err)
	}

	return id, nil
}

func (s *svc) GetOrCreateHiddenProject(ctx context.Context, workDir string) (int64, error) {
	id, err := s.store.GetOrCreateHiddenProject(ctx, workDir)
	if err != nil {
		return 0, fmt.Errorf("resolve hidden project: %w", err)
	}

	return id, nil
}

// EnsureManagementRoot delegates the atomic management-root ensure to the
// session store; the manager-bound controller resolves the hidden project.
func (s *svc) EnsureManagementRoot(
	ctx context.Context,
	projectID int64,
	owner string,
	topicID int64,
	name, workDir string,
) (*sessionstore.SessionRecord, *sessionstore.OutputCommit, error) {
	record, commit, err := s.managerRoots.EnsureManagementRoot(ctx, projectID, owner, topicID, name, workDir)
	if err != nil {
		return nil, nil, fmt.Errorf("ensure management root: %w", err)
	}

	return record, commit, nil
}

// ListHiddenProjectDirs exposes hidden project work dirs so /spawn navigation
// omits their directories without inferring hidden state from a basename.
func (s *svc) ListHiddenProjectDirs(ctx context.Context) ([]string, error) {
	rows, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list hidden projects: %w", err)
	}

	var dirs []string

	for _, row := range rows {
		if row.Hidden {
			dirs = append(dirs, row.WorkDir)
		}
	}

	return dirs, nil
}

func (s *svc) GetProjectWorkDir(ctx context.Context, projectID int64) (string, error) {
	workDir, err := s.store.GetProjectWorkDir(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("get project workdir: %w", err)
	}

	return workDir, nil
}

func (s *svc) GetProjectName(ctx context.Context, projectID int64) (string, error) {
	name, err := s.store.GetProjectName(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("get project name: %w", err)
	}

	return name, nil
}

func (s *svc) prepareStoppedSessionInput(
	ctx context.Context,
	record *sessionstore.SessionRecord,
	prompt string,
) (bool, error) {
	if record.Status != sessionstore.SessionStatusStopped {
		return false, nil
	}

	if !isReadOnlyBoundaryCommand(prompt) {
		if err := s.sessionStore.UpdateSessionStatus(
			ctx, record.ID, sessionstore.SessionStatusActive,
		); err != nil {
			return false, fmt.Errorf("resume stopped session %d: %w", record.ID, err)
		}

		return false, nil
	}

	commandOnly, err := s.commandOnlyStoppedRoot(ctx, record)
	if err != nil {
		return false, err
	}

	return !commandOnly, nil
}

// newProcessService shares the tree lock between process admission and stop.
func (s *svc) newProcessService(ctx context.Context) backgroundprocess.Service {
	fence := func(fenceCtx context.Context, rootSessionID int64) (func(), error) {
		unlock, err := s.lockSessionTree(fenceCtx, rootSessionID)
		if err != nil {
			return nil, fmt.Errorf("lock process tree %d: %w", rootSessionID, err)
		}

		if s.shuttingDown.Load() {
			unlock()

			return nil, backgroundprocess.ErrFenced
		}

		rec, err := s.sessionStore.GetSession(fenceCtx, rootSessionID)
		if err != nil {
			unlock()

			if errors.Is(err, sql.ErrNoRows) {
				return nil, backgroundprocess.ErrFenced
			}

			return nil, fmt.Errorf("fence session %d lookup: %w", rootSessionID, err)
		}

		if rec.Status == sessionstore.SessionStatusStopping ||
			rec.Status == sessionstore.SessionStatusTerminating ||
			rec.KilledAt != nil {
			unlock()

			return nil, backgroundprocess.ErrFenced
		}

		return unlock, nil
	}

	outputRoot, err := coagenthome.Join(coagenthome.ProcessesDirName)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.process").Warn("process_output_dir", zap.Error(err))

		return backgroundprocess.NewService(s.processStore, backgroundprocess.Options{
			TreeFence: fence,
		})
	}

	return backgroundprocess.NewService(s.processStore, backgroundprocess.Options{
		OutputDir: outputRoot,
		TreeFence: fence,
	})
}

func (s *svc) enqueueUserSessionInput(
	ctx context.Context,
	sessionID int64,
	prompt string,
) (*sessionstore.InboxInput, error) {
	attributes := make(map[string]any)
	switch strings.TrimSpace(prompt) {
	case "/schedules":
		content, err := s.schedulesCommand(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		attributes["schedules"] = content
	case "/status":
		current, err := s.CurrentProgress(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		attributes["status"] = current.Rendered
	}
	result, err := s.modelInputs.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:  sessionID,
			Source:     sessionstore.InputSourceUser,
			Content:    prompt,
			Attributes: attributes,
		},
	)
	if err != nil {
		return nil, err
	}
	return result.Input, nil
}

func (s *svc) stopRecovery() <-chan struct{} {
	return s.recovery.Close()
}

func (s *svc) beginProcessRecovery() func() {
	done := make(chan struct{})

	s.processRecoveryMu.Lock()
	s.processRecovery = done
	s.processRecoveryMu.Unlock()

	return func() {
		close(done)

		s.processRecoveryMu.Lock()
		if s.processRecovery == done {
			s.processRecovery = nil
		}
		s.processRecoveryMu.Unlock()
	}
}

func (s *svc) currentProcessRecovery() <-chan struct{} {
	s.processRecoveryMu.Lock()
	defer s.processRecoveryMu.Unlock()

	return s.processRecovery
}

// loadModelCatalog records the configured models once: the subagent picker reads
// the names, SetModel reads the effort levels.
func (s *svc) loadModelCatalog(models []config.ModelEntry) {
	s.modelEntries = models

	for _, m := range models {
		s.modelCatalog = append(s.modelCatalog, subagent.ModelInfo{ID: m.ID, Name: m.Name, Tags: m.Tags})
	}
}

// checkModelConfigured guards the idle path, where no live session validates.
// An empty catalog means no config was loaded, so it vouches for nothing.
func (s *svc) checkModelConfigured(model string) error {
	if len(s.modelCatalog) == 0 {
		return nil
	}

	for _, m := range s.modelCatalog {
		if m.ID == model {
			return nil
		}
	}

	return fmt.Errorf("unknown model: %s", model)
}

func (s *svc) removeSchedules(ctx context.Context, sessionID int64) {
	if s.scheduleSvc == nil {
		return
	}

	if err := s.scheduleSvc.RemoveAllForSession(ctx, sessionID); err != nil {
		logger.Ctx(ctx).Named("daemon.manager").
			Warn("remove_schedules_failed", zap.Int64("session_id", sessionID), zap.Error(err))
	}
}

func (s *svc) send(
	ctx context.Context,
	projectID int64,
	prompt, model string,
	attrs map[string]any,
) (int64, error) {
	workDir, err := s.store.GetProjectWorkDir(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("resolve project %d: %w", projectID, err)
	}

	if model == "" {
		model = s.defaultModel
	}

	level, err := s.resolveChildEffort(model, "", "")
	if err != nil {
		return 0, fmt.Errorf("resolve reasoning level for model %s: %w", model, err)
	}

	owner, _ := attrs[controllerapi.SessionAttributeManagerID].(string)
	var rec *sessionstore.SessionRecord
	createdWithInput := false

	if owner != "" {
		projectName, nameErr := s.store.GetProjectName(ctx, projectID)
		if nameErr != nil {
			return 0, fmt.Errorf("resolve project name: %w", nameErr)
		}

		rec, _, err = s.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
			ProjectID: projectID, Model: model, ReasoningLevel: level, Attributes: attrs,
			Prompt: prompt, StartEpisode: prompt != "" && !isExactControlCommand(prompt),
			Name: projectName, WorkDir: workDir,
		})
		if err != nil {
			return 0, fmt.Errorf("create manager session record: %w", err)
		}

		createdWithInput = prompt != ""
	} else {
		rec, err = s.sessionStore.CreateSession(ctx, projectID, model, level, attrs)
		if err != nil {
			return 0, fmt.Errorf("create session record: %w", err)
		}
	}

	if prompt != "" && !createdWithInput {
		if _, err := s.modelInputs.Enqueue(
			ctx,
			sessionstore.Input{SessionID: rec.ID, Source: sessionstore.InputSourceUser, Content: prompt},
		); err != nil {
			return 0, fmt.Errorf("persist initial session input: %w", err)
		}
	}

	if err := s.ensureRunner(ctx, rec.ID, workDir, projectID); err != nil {
		if errors.Is(err, admission.ErrNoCapacity) {
			s.enqueuePendingRunner(rec.ID, workDir, projectID)
			return rec.ID, nil
		}

		// Cleanup: ensureRunner failed after the session/root was already committed.
		// Mark the session killed so it doesn't appear alive to managers, even though
		// the worktree (for /gwt failures) may already be gone. This prevents
		// orphaned sessions in the store that reference deleted directories.
		// WithoutCancel: the kill marker must land even if the request context
		// died mid-launch.
		if _, killErr := s.lifecycleStore.MarkSessionKilledWithOutput(
			context.WithoutCancel(ctx),
			rec.ID,
			0,
		); killErr != nil {
			logger.Ctx(ctx).Named("daemon.manager").Warn("cleanup_orphaned_session",
				zap.Int64("session_id", rec.ID), zap.Error(killErr))
		}

		return 0, err
	}

	return rec.ID, nil
}
