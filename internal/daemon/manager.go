package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/backgroundprocess"
	budgetservice "github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/inputruntime"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionlifecycle"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

//nolint:interfacebloat // the daemon's whole operation surface; the Controller contract it backs is equally wide by design
type Service interface {
	Start(ctx context.Context) error
	Send(ctx context.Context, projectID int64, prompt, model string, attrs map[string]any) (int64, error)
	SendToSession(ctx context.Context, sessionID int64, prompt string) error
	SendToSessionResolved(ctx context.Context, sessionID int64, prompt string) (int64, error)
	DeliverPendingCallResult(
		ctx context.Context, sessionID int64, callID, toolName, content string,
	) (bool, error)
	ConsumeConfigEditActivation(ctx context.Context, sessionID int64, callID string)
	DeliverScheduleTick(ctx context.Context, sessionID int64, deliveryID, content string) (bool, error)
	DeliverFreshSchedule(ctx context.Context, sessionID int64, deliveryID, content string) (bool, error)
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
	supervisor       sessionlifecycle.Supervisor[queuedSessionInput]
	toolResources    tool.ResourceLifecycle
	budgetReconciler sessionlifecycle.BudgetReconciler
	factory          session.Factory
	store            Store
	sessionStore     sessionstore.OrchestrationStore
	inboxStore       sessionstore.InboxStore
	runtimeStore     sessionstore.AgentRuntimeStore
	managerOutputs   sessionstore.ManagerOutputStore
	managerRoots     sessionstore.ManagerRootTransactions
	lifecycleStore   sessionstore.SessionLifecycleStore
	modelInputs      sessionstore.ModelInputStore
	inputFactory     inputruntime.Factory
	links            subagent.Store
	subagents        subagent.Transactions
	scheduleSvc      schedule.Service
	routes           managerRoutes
	defaultModelFn   func() string
	modelCatalog     []modelInfo
	modelEntries     []config.ModelEntry
	// searchUnconfigured is the boot-time discoverability verdict: no
	// tools.search section and no native-capable model.
	searchUnconfigured bool
	mcpStore           mcpstore.Store
	externalCalls      externalCallCoordinator
	deferNotices       *deferAnnouncements
	shuttingDown       atomic.Bool
	recovery           sessionlifecycle.Recovery
	stopper            sessionlifecycle.Stopper
	completions        sessionlifecycle.Completions
	progress           progressruntime.Service
	budgetCtx          context.Context //nolint:containedctx // Daemon lifetime context for joined park workers.
	budgetCancel       context.CancelFunc
	budgetWG           sync.WaitGroup
	budgetParkMu       sync.Mutex
	budgetParks        map[budgetParkKey]struct{}
	budgetSvc          budgetservice.Service
	// processSvc owns live cancellation handles; processStore owns durability.
	processStore      backgroundprocess.Store
	processSvc        backgroundprocess.Service
	processRecoveryMu sync.Mutex
	processRecovery   chan struct{}
}

// OutputStore exposes the narrow manager-delivery ledger without widening the
// daemon's general Service interface used by controller fakes.
func (s *svc) OutputStore() sessionstore.ManagerOutputStore {
	return s.managerOutputs
}

func New(
	ctx context.Context,
	factory session.Factory,
	processStore backgroundprocess.Store,
	toolResources tool.ResourceLifecycle,
	store Store,
	sessionStore sessionstore.OrchestrationStore,
	inboxStore inputruntime.Store,
	runtimeStore sessionstore.AgentRuntimeStore,
	managerOutputs sessionstore.ManagerOutputStore,
	managerRoots sessionstore.ManagerRootTransactions,
	lifecycleStore sessionstore.SessionLifecycleStore,
	modelInputs sessionstore.ModelInputStore,
	links subagent.Store,
	subagents subagent.Transactions,
	budgetSvc budgetservice.Service,
	progressStore progressruntime.Store,
	scheduleSvc schedule.Service,
	cfg *config.Config,
	mcpStore mcpstore.Store,
	applier configapply.Service,
) (Service, error) {
	s, err := newSvc(
		ctx,
		factory, processStore, toolResources, store, sessionStore, inboxStore, runtimeStore,
		managerOutputs, managerRoots, lifecycleStore, modelInputs,
		links, subagents, budgetSvc, progressStore,
		scheduleSvc, cfg.DefaultModel, applier,
	)
	if err != nil {
		return nil, fmt.Errorf("create daemon: %w", err)
	}

	s.mcpStore = mcpStore
	s.searchUnconfigured = searchUnconfigured(cfg.UnifiedConfig)

	if cfg.UnifiedConfig != nil {
		s.loadModelCatalog(cfg.UnifiedConfig.Models)
	}

	return s, nil
}

func newSvc(
	ctx context.Context,
	factory session.Factory,
	processStore backgroundprocess.Store,
	toolResources tool.ResourceLifecycle,
	store Store,
	sessionStore sessionstore.OrchestrationStore,
	inboxStore inputruntime.Store,
	runtimeStore sessionstore.AgentRuntimeStore,
	managerOutputs sessionstore.ManagerOutputStore,
	managerRoots sessionstore.ManagerRootTransactions,
	lifecycleStore sessionstore.SessionLifecycleStore,
	modelInputs sessionstore.ModelInputStore,
	links subagent.Store,
	subagents subagent.Transactions,
	budgetSvc budgetservice.Service,
	progressStore progressruntime.Store,
	scheduleSvc schedule.Service,
	defaultModelFn func() string,
	applier configapply.Service,
) (*svc, error) {
	if toolResources == nil {
		return nil, errors.New("daemon requires tool resources")
	}

	budgetCtx, budgetCancel := context.WithCancel(context.Background())
	s := &svc{
		factory:        factory,
		processStore:   processStore,
		toolResources:  toolResources,
		store:          store,
		sessionStore:   sessionStore,
		inboxStore:     inboxStore,
		runtimeStore:   runtimeStore,
		managerOutputs: managerOutputs,
		managerRoots:   managerRoots,
		lifecycleStore: lifecycleStore,
		modelInputs:    modelInputs,
		inputFactory:   inputruntime.New(inboxStore, scheduleSvc),
		links:          links,
		subagents:      subagents,
		budgetSvc:      budgetSvc,
		scheduleSvc:    scheduleSvc,
		recovery:       sessionlifecycle.NewRecovery(),
		stopper: sessionlifecycle.NewStopper(
			sessionStore, lifecycleStore, managerOutputs, links,
		),
		routes:         newManagerRoutes(sessionStore, managerRoots, store, sessionbus.New()),
		defaultModelFn: defaultModelFn,
		deferNotices:   newDeferAnnouncements(),
		budgetCtx:      budgetCtx,
		budgetCancel:   budgetCancel,
	}

	s.externalCalls = newExternalCalls(
		applier, inboxStore, sessionStore, runtimeStore, scheduleSvc, links,
		func(ctx context.Context, sessionID int64, input pendingCallResultInput) error {
			return s.enqueueSessionInput(ctx, sessionID, input)
		},
	)

	s.progress = newProgressRuntime(progressStore, s)
	if budgetSvc != nil {
		s.budgetReconciler = sessionlifecycle.NewBudgetReconciler(budgetSvc, s.startBudgetPark)
	}

	s.completions = s.newCompletionCoordinator()

	s.supervisor = sessionlifecycle.NewSupervisor(
		sessionStore, links, s.ensureRunnerStartable, s.childTerminated, s.runSession,
	)
	if processStore != nil {
		s.processSvc = s.newProcessService(ctx)
	}

	return s, nil
}

func (s *svc) PubSub() sessionbus.Source {
	return s.routes.Source()
}

func (s *svc) NotifySession(sessionID int64, n sessionevent.Notification) {
	s.routes.Publish(sessionID, n)
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
		if errors.Is(err, sessionstore.ErrBudgetConflict) {
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

	_, ok := s.supervisor.Lookup(sessionID)
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

	if _, ok = s.supervisor.Lookup(sessionID); ok {
		return nil
	}

	if err := s.ensureRunner(ctx, sessionID, workDir, rec.ProjectID, nil); err != nil {
		if errors.Is(err, admission.ErrNoCapacity) {
			s.enqueuePendingRunner(sessionID, workDir, rec.ProjectID)
			return nil
		}

		return err
	}

	return nil
}

func (s *svc) SendToSessionResolved(ctx context.Context, sessionID int64, prompt string) (int64, error) {
	resolved, err := s.routes.ResolveReplacement(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("resolve replacement for session %d: %w", sessionID, err)
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
		isCompactCommand(content)
}

func isCompactCommand(content string) bool {
	content = strings.TrimSpace(content)

	return content == compactCommand || strings.HasPrefix(content, compactCommand+" ")
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

	if input.RawContent != stopCommand && input.RawContent != clearCommand && input.RawContent != killCommand {
		return false, nil
	}

	unlock, err := s.lockSessionTree(ctx, input.SessionID)
	if err != nil {
		return true, err
	}
	defer unlock()

	switch input.RawContent {
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
		if _, err := s.routes.Replace(ctx, input.SessionID, input.ID, s.killLocked); err != nil {
			return true, fmt.Errorf("clear session %d: %w", input.SessionID, err)
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
		return fmt.Errorf("capture status progress: %w", err)
	}

	if _, owned := input.Attributes[controllerapi.SessionAttributeManagerID].(string); owned {
		_, err = s.lifecycleStore.HandleInputWithOutput(ctx, input.ID, "status command", sessionstore.OutputDraft{
			SessionID: input.SessionID,
			Type:      sessionstore.OutputMessagePersistent,
			Content:   current.Rendered,
		})
	} else {
		err = s.inboxStore.HandleInput(ctx, input.ID, "status command")
	}

	if err != nil {
		return fmt.Errorf("handle status input: %w", err)
	}

	s.routes.Publish(input.SessionID, sessionevent.Notification{
		Type: sessionevent.NotifyMessage, Message: current.Rendered,
	})
	s.routes.Publish(input.SessionID, sessionevent.Notification{
		Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle,
	})

	return nil
}

//nolint:funcorder // The idempotent stop result is part of the same command dispatcher.
func (s *svc) handleStoppedStop(ctx context.Context, input *sessionstore.InboxInput) error {
	content := "Session already stopped."

	if _, owned := input.Attributes[controllerapi.SessionAttributeManagerID].(string); owned {
		_, err := s.lifecycleStore.HandleInputWithOutput(ctx, input.ID, "stop command", sessionstore.OutputDraft{
			SessionID: input.SessionID,
			Type:      sessionstore.OutputMessagePersistent,
			Content:   content,
			SourceKey: fmt.Sprintf("input:%d:stop:already_stopped", input.ID),
			Fingerprint: sessionstore.OutputFingerprint(
				sessionstore.OutputMessagePersistent,
				content,
				input.SessionID,
				nil,
			),
		})
		if err != nil {
			return fmt.Errorf("handle stopped stop with output: %w", err)
		}

		return nil
	}

	if err := s.inboxStore.HandleInput(ctx, input.ID, "stop command"); err != nil {
		return fmt.Errorf("handle stopped stop: %w", err)
	}

	return s.enqueuePersistentOutput(ctx, input.SessionID, content)
}

//nolint:funcorder // Lifecycle input must stay with the generic dispatcher that invokes it.
func (s *svc) handleLifecycleInput(ctx context.Context, input *sessionstore.InboxInput, content string) error {
	command := strings.TrimPrefix(input.RawContent, "/")

	if _, owned := input.Attributes[controllerapi.SessionAttributeManagerID].(string); !owned {
		if err := s.inboxStore.HandleInput(ctx, input.ID, command); err != nil {
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

// DeliverPendingCallResult answers one specific pending tool call. It is how an
// outcome produced outside the loop — a config verdict that survived a restart,
// a secret typed at a terminal — gets back into the session that asked for it,
// whatever channel that session belongs to.
func (s *svc) DeliverPendingCallResult(
	ctx context.Context, sessionID int64, callID, toolName, content string,
) (bool, error) {
	return s.deliverSessionInput(ctx, sessionID, pendingCallResultInput{
		Call:    session.PendingToolCall{ID: callID, Name: toolName},
		Content: content,
	})
}

func (s *svc) DeliverScheduleTick(
	ctx context.Context,
	sessionID int64,
	deliveryID, content string,
) (bool, error) {
	if root, err := s.isRootScheduleTarget(ctx, sessionID); err != nil || !root {
		return false, err
	}

	return s.deliverSessionInput(ctx, sessionID, scheduleTickInput{
		DeliveryID: deliveryID,
		Content:    content,
	})
}

func (s *svc) DeliverFreshSchedule(
	ctx context.Context,
	sessionID int64,
	deliveryID, content string,
) (bool, error) {
	if root, err := s.isRootScheduleTarget(ctx, sessionID); err != nil || !root {
		return false, err
	}

	return s.deliverSessionInput(ctx, sessionID, freshScheduleInput{
		DeliveryID: deliveryID,
		Prompt:     content,
	})
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
	_, ok := s.supervisor.Lookup(sessionID)

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
	rs, ok := s.supervisor.Lookup(sessionID)

	if ok {
		s.routes.Publish(
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
		s.routes.Publish(sessionID, sessionevent.Notification{
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
		s.routes.Publish(sessionID, sessionevent.Notification{
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
		s.routes.Publish(sessionID, sessionevent.Notification{
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
	settleParkedCompactions     bool
	cancelledProcesses          *int
}

//nolint:funcorder // The second stop phase belongs beside the public Stop transition.
func (s *svc) stopTreeCleanup(ctx context.Context, sessionID int64, options stopTreeOptions) error {
	effects := sessionlifecycle.StopEffects{
		CancelProcesses:  func(ctx context.Context, id int64) error { return s.stopTreeBackgroundProcesses(ctx, id, options) },
		RetireResources:  s.retireTreeToolResources,
		SettleCalls:      s.externalCalls.SettleStopped,
		ExpireActivation: s.externalCalls.ExpireActivation,
		CancelSleeps:     s.cancelStoppedSleeps,
	}
	if options.settleParkedCompactions {
		effects.SettleControlInputs = s.settleParkedCompactions
	}

	err := s.supervisor.StopTree(ctx, sessionID, s.stopper, options.keepRootStopping, effects)
	if err != nil {
		return fmt.Errorf("stop session tree: %w", err)
	}

	return nil
}

// ConsumeConfigEditActivation also settles a committed apply recovered at boot.
func (s *svc) ConsumeConfigEditActivation(ctx context.Context, sessionID int64, callID string) {
	s.externalCalls.ConsumeActivation(ctx, sessionID, callID)
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

func (s *svc) Clear(ctx context.Context, sessionID int64) (int64, error) {
	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	defer unlock()

	replacementID, err := s.routes.Replace(ctx, sessionID, 0, s.killLocked)
	if err != nil {
		return 0, fmt.Errorf("clear session %d: %w", sessionID, err)
	}

	return replacementID, nil
}

// SetModel applies the switch before recording it: a model the session cannot
// run must never land in the record, or the session stops being resumable.
func (s *svc) SetModel(ctx context.Context, sessionID int64, model, reasoningLevel string) error {
	if err := s.checkModelConfigured(model); err != nil {
		return err
	}

	if s.budgetSvc != nil {
		record, loadErr := s.sessionStore.GetSession(ctx, sessionID)
		if loadErr != nil {
			return fmt.Errorf("load session for budgeted model switch: %w", loadErr)
		}

		budgetRecord, budgetErr := s.budgetSvc.Get(ctx, sessionRootID(record))
		if budgetErr == nil && budgetRecord.State == sessionstore.BudgetArmed &&
			budgetRecord.CostLimitUSD != nil && !s.modelHasPricing(model) {
			return errors.New("cannot switch an armed budget tree to a model without catalog pricing")
		}

		if budgetErr != nil && !errors.Is(budgetErr, sessionstore.ErrBudgetNotFound) {
			return fmt.Errorf("load budget for model switch: %w", budgetErr)
		}
	}

	// The record is all a later run reads, so it must carry the level a session
	// would settle on: the model's default when none is asked for, none at all
	// for a model with no effort selector.
	level := reasoningLevel

	if len(s.modelEntries) > 0 {
		resolved, err := session.ResolveReasoningLevel(s.modelEntries, model, reasoningLevel)
		if err != nil {
			return fmt.Errorf("switch session %d to model %s: %w", sessionID, model, err)
		}

		level = resolved
	}

	rs, ok := s.supervisor.Lookup(sessionID)

	var sessSvc session.Service

	if ok {
		sessSvc = rs.Service()
	}

	if sessSvc != nil {
		if err := sessSvc.SetModel(model, level); err != nil {
			return fmt.Errorf("switch session %d to model %s: %w", sessionID, model, err)
		}
	}

	if err := s.sessionStore.UpdateSessionModel(ctx, sessionID, model, level); err != nil {
		return fmt.Errorf("update session model: %w", err)
	}

	return nil
}

func (s *svc) SetAttributes(ctx context.Context, sessionID int64, attrs map[string]any) error {
	if err := s.routes.SetAttributes(ctx, sessionID, attrs); err != nil {
		return fmt.Errorf("set attributes for session %d: %w", sessionID, err)
	}

	return nil
}

func (s *svc) Shutdown(timeout time.Duration) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	s.shuttingDown.Store(true)

	if s.budgetCancel != nil {
		s.budgetCancel()
	}

	recoveryDone := s.stopRecovery()
	processRecoveryDone := s.currentProcessRecovery()

	done := make(chan struct{})

	go func() {
		s.supervisor.Shutdown(func() {
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

			if s.budgetReconciler != nil {
				_ = s.budgetReconciler.Stop(shutdownCtx)
			}
		}, func() {
			if recoveryDone != nil {
				<-recoveryDone
			}

			if processRecoveryDone != nil {
				<-processRecoveryDone
			}

			s.budgetWG.Wait()

			if err := s.toolResources.Close(); err != nil {
				logger.Named("manager.shutdown").Warn("close_tool_resources", zap.Error(err))
			}
		})
		close(done)
	}()

	select {
	case <-done:
	case <-shutdownCtx.Done():
		logger.Named("manager.shutdown").Warn("shutdown_timeout", zap.Int("remaining_sessions", s.supervisor.Count()))
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
		OnCompletion: func(ctx context.Context, completion backgroundprocess.Completion) {
			s.routeProcessCompletion(ctx, completion)
		},
		TreeFence: fence,
	})
}

func (s *svc) enqueueUserSessionInput(
	ctx context.Context,
	sessionID int64,
	prompt string,
) (*sessionstore.InboxInput, error) {
	if isExactControlCommand(prompt) {
		return s.enqueueGenericUserInput(ctx, sessionID, prompt)
	}

	record, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load session for model input: %w", err)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if record.ParentID != 0 || owner == "" {
		return s.enqueueGenericUserInput(ctx, sessionID, prompt)
	}

	input, err := s.modelInputs.EnqueueModelInput(ctx, sessionID, prompt)
	if err != nil {
		return nil, fmt.Errorf("enqueue model input: %w", err)
	}

	return input, nil
}

func (s *svc) enqueueGenericUserInput(
	ctx context.Context,
	sessionID int64,
	prompt string,
) (*sessionstore.InboxInput, error) {
	input, err := s.inboxStore.EnqueueInput(ctx, sessionID, sessionstore.InputSourceUser, prompt)
	if err != nil {
		return nil, fmt.Errorf("enqueue generic user input: %w", err)
	}

	return input, nil
}

func (s *svc) isRootScheduleTarget(ctx context.Context, sessionID int64) (bool, error) {
	rec, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("session %d not found", sessionID)
	}

	return rec.ParentID == 0, nil
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
		s.modelCatalog = append(s.modelCatalog, modelInfo{ID: m.ID, Name: m.Name, Tags: m.Tags})
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

// appendIfLive appends the input to the session's live runner under the
// registry lock, returning true if a live runner existed. Holding the lock across
// the append serializes it with runner teardown (delete + leftover drain).
func (s *svc) appendIfLive(sessionID int64, input queuedSessionInput) bool {
	return s.supervisor.Append(sessionID, input)
}

func (s *svc) deliverSessionInput(ctx context.Context, sessionID int64, input sessionInput) (bool, error) {
	if err := input.validate(); err != nil {
		return false, err
	}

	delivery := newAwaitedSessionInput(input)
	if err := s.routeQueuedSessionInput(ctx, sessionID, delivery); err != nil {
		delivery.complete(false, err)
		return false, err
	}

	select {
	case outcome := <-delivery.done:
		return outcome.Applied, outcome.Err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (s *svc) enqueueSessionInput(ctx context.Context, sessionID int64, input sessionInput) error {
	if err := input.validate(); err != nil {
		return err
	}

	return s.routeQueuedSessionInput(ctx, sessionID, asyncSessionInput{value: input})
}

// routeQueuedSessionInput appends a validated delivery to a live runner or
// lazily revives an idle session. It rejects killed sessions; awaited callers
// receive the actual injection outcome through their delivery object.
func (s *svc) routeQueuedSessionInput(ctx context.Context, sessionID int64, input queuedSessionInput) error {
	return s.routeQueuedSessionInputWithEnsure(ctx, sessionID, input, s.ensureRunner)
}

func (s *svc) routeQueuedSessionInputLocked(
	ctx context.Context,
	sessionID int64,
	input queuedSessionInput,
) error {
	return s.routeQueuedSessionInputWithEnsure(ctx, sessionID, input, s.ensureRunnerLocked)
}

func (s *svc) routeQueuedSessionInputWithEnsure(
	ctx context.Context,
	sessionID int64,
	input queuedSessionInput,
	ensure func(context.Context, int64, string, int64, []queuedSessionInput) error,
) error {
	if err := input.input().validate(); err != nil {
		input.complete(false, err)
		return err
	}

	rec, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session %d not found", sessionID)
	}

	if rec.KilledAt != nil {
		return fmt.Errorf("session %d is killed", sessionID)
	}

	if rec.Status == sessionstore.SessionStatusStopping {
		return fmt.Errorf("session %d is %s", sessionID, rec.Status)
	}

	if rec.Status == sessionstore.SessionStatusStopped &&
		(rec.ParentID != 0 || !inputIsScheduledTurn(input.input())) {
		return fmt.Errorf("session %d is %s", sessionID, rec.Status)
	}

	// Registry serialization prevents teardown from losing an input appended
	// before entry deletion and the final leftover drain.
	if s.appendIfLive(sessionID, input) {
		return nil
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	if err != nil {
		return fmt.Errorf("resolve project %d: %w", rec.ProjectID, err)
	}

	if s.appendIfLive(sessionID, input) {
		return nil
	}

	return ensure(ctx, sessionID, workDir, rec.ProjectID, []queuedSessionInput{input})
}

// removeSchedules deletes all schedules (one-shot and cron) for a killed session.
// Cleanup failure must not fail the kill — the session is already terminal.
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

	if model == "" && s.defaultModelFn != nil {
		model = s.defaultModelFn()
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
		if _, err := s.inboxStore.EnqueueInput(ctx, rec.ID, sessionstore.InputSourceUser, prompt); err != nil {
			return 0, fmt.Errorf("persist initial session input: %w", err)
		}
	}

	if err := s.ensureRunner(ctx, rec.ID, workDir, projectID, nil); err != nil {
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

func (s *svc) cancelStoppedSleeps(ctx context.Context, id int64) error {
	if s.scheduleSvc == nil {
		return nil
	}

	if _, err := s.scheduleSvc.CancelPendingSleeps(ctx, id); err != nil {
		return fmt.Errorf("cancel one-shot waits for session %d: %w", id, err)
	}

	return nil
}
