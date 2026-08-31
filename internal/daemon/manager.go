package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/admission"
	budgetservice "github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

//nolint:interfacebloat // the daemon's whole operation surface; the Controller contract it backs is equally wide by design
type Service interface {
	Start(ctx context.Context) error
	Send(ctx context.Context, projectID int64, prompt, model string, attrs map[string]any) (int64, error)
	SendToSession(ctx context.Context, sessionID int64, prompt string) error
	DeliverPendingCallResult(
		ctx context.Context, sessionID int64, callID, toolName, content string,
	) (bool, error)
	DeliverScheduleTick(ctx context.Context, sessionID int64, deliveryID, content string) (bool, error)
	DeliverFreshSchedule(ctx context.Context, sessionID int64, deliveryID, content string) (bool, error)
	ResolveSecretRequest(ctx context.Context, requestID, name string) error
	CancelSecretRequest(ctx context.Context, requestID string) error
	PendingSecretRequests(sessionID int64) []sessionevent.Notification
	Kill(ctx context.Context, sessionID int64) error
	Stop(ctx context.Context, sessionID, inputID int64) error
	Clear(ctx context.Context, sessionID int64) (int64, error)
	SetModel(ctx context.Context, sessionID int64, model, reasoningLevel string) error
	SetAttributes(ctx context.Context, sessionID int64, attrs map[string]any) error
	GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error)
	List(ctx context.Context) ([]*sessionstore.SessionRecord, error)
	HasActiveLoop(sessionID int64) bool
	PubSub() sessionbus.Source
	NotifySession(sessionID int64, n sessionevent.Notification)
	Shutdown(timeout time.Duration)
	GetOrCreateProject(ctx context.Context, workDir string) (int64, error)
	GetOrCreateSystemProject(ctx context.Context, workDir, name string) (int64, error)
	GetProjectWorkDir(ctx context.Context, projectID int64) (string, error)
	GetProjectName(ctx context.Context, projectID int64) (string, error)
	ListRecentProjects(ctx context.Context, root string) ([]RecentProject, error)
}

// LegacyCLIPreparer claims unambiguous pre-owner local-chat roots before the
// recovery sweep can construct a root session without durable output ownership.
//
//nolint:iface // composition root asserts the preparation capability structurally.
type LegacyCLIPreparer interface {
	PrepareLegacyCLIRoots(ctx context.Context) error
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

	errDaemonShuttingDown = errors.New("daemon is shutting down")
)

type svc struct {
	mu             sync.Mutex
	loops          map[int64]*runner
	factory        session.Factory
	store          Store
	sessionStore   sessionstore.OrchestrationStore
	inboxStore     sessionstore.InboxStore
	links          subagent.Store
	subagents      subagent.Transactions
	scheduleSvc    schedule.Service
	admit          admission.Governor
	queueMu        sync.Mutex
	queue          []queuedChild
	pendingMu      sync.Mutex
	pendingRunners []queuedRunner
	pubsub         sessionbus.Bus
	defaultModelFn func() string
	modelCatalog   []modelInfo
	modelEntries   []config.ModelEntry
	mcpStore       mcpstore.Store
	mcpPool        mcp.Pool
	applier        configapply.Service
	staged         *stagedCalls
	secrets        *secretRequests
	deferNotices   *deferAnnouncements
	systemProject  string
	shuttingDown   atomic.Bool
	recoveryMu     sync.Mutex
	recoveryCancel context.CancelFunc
	recoveryDone   chan struct{}
	progressCancel context.CancelFunc
	progressDone   chan struct{}
	progressWake   chan struct{}
	progressMu     sync.Mutex
	progressNow    func() time.Time
	progressTimer  func(time.Duration) progressTimer
	budgetCtx      context.Context //nolint:containedctx // Daemon lifetime context for joined park workers.
	budgetCancel   context.CancelFunc
	budgetWG       sync.WaitGroup
	// treeMu linearizes subagent admission with the durable stop boundary.
	treeMu sync.Locker
	// routeMu linearizes owner claims with replacement-session creation. The
	// daemon is single-instance, so this is the ownership CAS boundary.
	routeMu sync.Mutex
	// childMu guards the publication route caches only — never s.mu,
	// which is held around runner starts and must not be contended by publish.
	childMu    sync.Mutex
	childCache map[int64]bool
	ownerCache map[int64]string
	budgetSvc  budgetservice.Service
}

// OutputStore exposes the narrow manager-delivery ledger without widening the
// daemon's general Service interface used by controller fakes.
func (s *svc) OutputStore() sessionstore.OutputStore {
	store, _ := s.sessionStore.(sessionstore.OutputStore)
	return store
}

func (s *svc) PrepareLegacyCLIRoots(ctx context.Context) error {
	store, ok := s.sessionStore.(sessionstore.LegacyCLIClaimStore)
	if !ok {
		return nil
	}

	if err := store.ClaimLegacyCLIRoots(
		ctx,
		controllerapi.CoagentSystemProjectName,
		s.systemProject,
		"cli",
		controllerapi.BuiltinCLIManagerID,
	); err != nil {
		return fmt.Errorf("claim legacy cli roots: %w", err)
	}

	return nil
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
	factory session.Factory,
	store Store,
	sessionStore sessionstore.OrchestrationStore,
	inboxStore sessionstore.InboxStore,
	links subagent.Store,
	subagents subagent.Transactions,
	scheduleSvc schedule.Service,
	cfg *config.Config,
	mcpStore mcpstore.Store,
	mcpPool mcp.Pool,
	applier configapply.Service,
) Service {
	s := newSvc(factory, store, sessionStore, inboxStore, links, subagents, scheduleSvc, cfg.DefaultModel)
	s.systemProject = filepath.Join(
		resolveProjectsRoot(cfg.UnifiedConfig),
		controllerapi.CoagentSystemProjectDir,
	)
	s.mcpStore = mcpStore
	s.mcpPool = mcpPool
	s.applier = applier

	if cfg.UnifiedConfig != nil {
		s.loadModelCatalog(cfg.UnifiedConfig.Models)
	}

	return s
}

//nolint:wsl_v5 // Lifetime contexts are created immediately before service assembly.
func newSvc(
	factory session.Factory,
	store Store,
	sessionStore sessionstore.OrchestrationStore,
	inboxStore sessionstore.InboxStore,
	links subagent.Store,
	subagents subagent.Transactions,
	scheduleSvc schedule.Service,
	defaultModelFn func() string,
) *svc {
	budgetCtx, budgetCancel := context.WithCancel(context.Background())
	s := &svc{
		loops:          make(map[int64]*runner),
		factory:        factory,
		store:          store,
		sessionStore:   sessionStore,
		inboxStore:     inboxStore,
		links:          links,
		subagents:      subagents,
		scheduleSvc:    scheduleSvc,
		staged:         newStagedCalls(),
		secrets:        newSecretRequests(),
		admit:          admission.New(),
		pubsub:         sessionbus.New(),
		treeMu:         &sync.Mutex{},
		defaultModelFn: defaultModelFn,
		childCache:     make(map[int64]bool),
		ownerCache:     make(map[int64]string),
		deferNotices:   newDeferAnnouncements(),
		progressWake:   make(chan struct{}, 1),
		progressNow:    time.Now,
		progressTimer:  newRealProgressTimer,
		budgetCtx:      budgetCtx,
		budgetCancel:   budgetCancel,
	}
	if budgetStore, ok := sessionStore.(sessionstore.BudgetStore); ok {
		s.budgetSvc = budgetservice.New(budgetStore)
	}

	return s
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

	s.mu.Lock()
	_, ok := s.loops[sessionID]
	s.mu.Unlock()

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

	if rec.Status == sessionstore.SessionStatusStopped && !isReadOnlyBoundaryCommand(prompt) {
		if err := s.sessionStore.UpdateSessionStatus(ctx, sessionID, sessionstore.SessionStatusActive); err != nil {
			return fmt.Errorf("resume stopped session %d: %w", sessionID, err)
		}
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	if err != nil {
		return fmt.Errorf("resolve project %d: %w", rec.ProjectID, err)
	}

	s.mu.Lock()
	if _, ok = s.loops[sessionID]; ok {
		s.mu.Unlock()

		return nil
	}
	s.mu.Unlock()

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
	record, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("load session for replacement resolution: %w", err)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if replacements, ok := s.sessionStore.(sessionstore.ReplacementStore); ok {
		resolved, err := replacements.ResolveReplacement(ctx, sessionID, owner)
		if err != nil {
			return 0, fmt.Errorf("resolve replacement session: %w", err)
		}

		sessionID = resolved
	}

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

	if input.RawContent != stopCommand && input.RawContent != clearCommand && input.RawContent != killCommand {
		return false, nil
	}

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

		return true, s.Stop(ctx, input.SessionID, input.ID)
	case clearCommand:
		if _, err := s.clear(ctx, input.SessionID, input.ID); err != nil {
			return true, err
		}

		return true, nil
	case killCommand:
		if err := s.handleLifecycleInput(ctx, input, "Stopping session..."); err != nil {
			return true, err
		}

		return true, s.Kill(ctx, input.SessionID)
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
		if outputs, ok := s.inboxStore.(sessionstore.CommandOutputStore); ok {
			_, err = outputs.HandleInputWithOutput(ctx, input.ID, "status command", sessionstore.OutputDraft{
				SessionID: input.SessionID,
				Type:      sessionstore.OutputMessagePersistent,
				Content:   current.Rendered,
			})
		} else {
			err = s.inboxStore.HandleInput(ctx, input.ID, "status command")
		}
	} else {
		err = s.inboxStore.HandleInput(ctx, input.ID, "status command")
	}

	if err != nil {
		return fmt.Errorf("handle status input: %w", err)
	}

	s.publish(input.SessionID, sessionevent.Notification{
		Type: sessionevent.NotifyMessage, Message: current.Rendered,
	})

	return nil
}

//nolint:funcorder // The idempotent stop result is part of the same command dispatcher.
func (s *svc) handleStoppedStop(ctx context.Context, input *sessionstore.InboxInput) error {
	content := "Session already stopped."

	if _, owned := input.Attributes[controllerapi.SessionAttributeManagerID].(string); owned {
		if outputs, ok := s.inboxStore.(sessionstore.CommandOutputStore); ok {
			_, err := outputs.HandleInputWithOutput(ctx, input.ID, "stop command", sessionstore.OutputDraft{
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

	outputs, ok := s.inboxStore.(sessionstore.LifecycleCommandStore)
	if !ok {
		if err := s.inboxStore.HandleInput(ctx, input.ID, command); err != nil {
			return fmt.Errorf("handle lifecycle input: %w", err)
		}

		return s.enqueuePersistentOutput(ctx, input.SessionID, content)
	}

	if _, err := outputs.BeginLifecycleInput(ctx, input.ID, command, content); err != nil {
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

// StageExternalCall records that a tool call suspended awaiting work the daemon
// itself is doing, so the loop neither re-executes it nor advances past it.
func (s *svc) StageExternalCall(sessionID int64, callID, toolName string) {
	s.staged.stage(sessionID, callID, toolName)
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
	s.mu.Lock()
	_, ok := s.loops[sessionID]
	s.mu.Unlock()

	return ok
}

func (s *svc) Kill(ctx context.Context, sessionID int64) error {
	s.mu.Lock()
	rs, ok := s.loops[sessionID]
	s.mu.Unlock()

	if ok {
		s.publish(
			sessionID,
			sessionevent.Notification{
				Type:    sessionevent.NotifyMessage,
				Message: "Stopping session...",
			},
		)

		rs.stop()
	}

	rec, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session %d not found", sessionID)
	}

	if rec.KilledAt != nil {
		return nil
	}

	if rec.ParentID == 0 {
		if err := s.releaseArmedBudget(ctx, sessionID, "killed"); err != nil {
			return err
		}
	}

	// Cleanup must complete even if the caller disconnects mid-Kill — detach
	// from request-scoped cancellation while keeping logger values.
	cleanupCtx := context.WithoutCancel(ctx)

	if lifecycle, ok := s.sessionStore.(sessionstore.LifecycleOutputStore); ok {
		if _, err := lifecycle.MarkSessionKilledWithOutput(cleanupCtx, sessionID); err != nil {
			return fmt.Errorf("mark session killed with output: %w", err)
		}
	} else if err := s.sessionStore.MarkSessionKilled(cleanupCtx, sessionID); err != nil {
		return fmt.Errorf("mark session killed: %w", err)
	}

	_, _ = s.inboxStore.CancelPendingInputs(cleanupCtx, []int64{sessionID}, "killed")
	s.removeSchedules(cleanupCtx, sessionID)

	// Cascade-kill every non-terminal descendant (blocking and background): this is
	// a deliberate tree teardown, so background work that would outlive it and
	// report to nobody is stopped too. Completed-but-undelivered children keep their
	// result (see cascadeKillChildren).
	s.cascadeKillChildren(cleanupCtx, sessionID, 0, time.Now().Add(cascadeRetryBudget))

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

	if err := s.stopTreeCleanup(ctx, sessionID, explicit); err != nil {
		return err
	}

	if explicit {
		return s.completeExplicitStop(ctx, sessionID, inputID)
	}

	if err := s.releaseArmedBudget(ctx, sessionID, "stopped"); err != nil {
		return err
	}

	if err := s.convergeOrphanedStopStart(ctx, sessionID, inputID); err != nil {
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
func (s *svc) convergeOrphanedStopStart(ctx context.Context, sessionID, inputID int64) error {
	if inputID <= 0 {
		return nil
	}

	record, recordErr := s.sessionStore.GetSession(ctx, sessionID)
	// Ownerless stops have no start row to converge; an unread session stays a
	// startup-recovery case rather than a terminal fact published blind.
	if recordErr == nil && !ownerlessSession(record) {
		return s.completeExplicitStop(ctx, sessionID, inputID)
	}

	return nil
}

// completeExplicitStop commits the terminal stop fact and then issues a
// non-authoritative delivery wake, so the manager need not wait for its idle
// rescan. The outbox remains the source of truth either way.
//
//nolint:funcorder // belongs beside the public Stop transition it completes.
func (s *svc) completeExplicitStop(ctx context.Context, rootID, inputID int64) error {
	store, ok := s.sessionStore.(sessionstore.StopCompletionStore)
	if !ok {
		return errors.New("stop completion store unavailable")
	}

	if _, err := store.CompleteExplicitStop(ctx, rootID, inputID); err != nil {
		return fmt.Errorf("commit explicit stop completion: %w", err)
	}

	record, err := s.sessionStore.GetSession(ctx, rootID)
	if err != nil {
		return nil //nolint:nilerr // the terminal fact is committed; the wake is best-effort
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if outputs := s.OutputStore(); outputs != nil && owner != "" {
		_, _ = outputs.WakeOutputHead(ctx, owner)
	}

	return nil
}

// stopTreeCleanup durably parks a tree without publishing user-command events.
// Startup recovery and budget parking use it to finish an interrupted stop
// without replaying UI. With keepRootStopping the explicit root stays in its
// stopping fence: the caller owns the single terminal transaction that moves it
// to `stopped` together with the visible completion output.
//
//nolint:funcorder // The second stop phase belongs beside the public Stop transition.
func (s *svc) stopTreeCleanup(ctx context.Context, sessionID int64, keepRootStopping bool) error {
	s.treeMu.Lock()
	cleanupCtx := context.WithoutCancel(ctx)

	ids, links, err := s.stopTree(cleanupCtx, sessionID)
	if err != nil {
		s.treeMu.Unlock()
		return err
	}

	// Closing admission before cancellation prevents queued descendants from
	// being launched while the tree is being parked.
	for _, id := range ids {
		if err := s.sessionStore.UpdateSessionStatus(cleanupCtx, id, sessionstore.SessionStatusStopping); err != nil {
			s.treeMu.Unlock()
			return fmt.Errorf("mark session %d stopping: %w", id, err)
		}
	}

	for _, link := range links {
		if err := s.links.MarkLinkStopped(cleanupCtx, link.ChildID); err != nil {
			s.treeMu.Unlock()
			return fmt.Errorf("mark subagent %d stopped: %w", link.ChildID, err)
		}
	}
	s.treeMu.Unlock()

	s.removeQueuedSessions(ids)

	runners := make([]*runner, 0, len(ids))
	for _, id := range ids {
		s.mu.Lock()
		rs := s.loops[id]
		s.mu.Unlock()

		if rs != nil {
			runners = append(runners, rs)
		}
	}

	// Signal the entire tree before waiting for any one runner: a foreground
	// parent can otherwise keep a child alive while stop is waiting on it.
	for _, rs := range runners {
		rs.cancel()
	}

	for _, rs := range runners {
		<-rs.done
	}

	if _, err := s.inboxStore.CancelPendingInputs(cleanupCtx, ids, "stopped"); err != nil {
		return fmt.Errorf("cancel stopped session input: %w", err)
	}

	// With all writers joined, it is safe to close every outstanding tool_use in
	// the transcript. This is what makes a stopped session resumable without
	// replaying a sleep/config/task call that no longer exists.
	for _, id := range ids {
		if err := s.settleStoppedCalls(cleanupCtx, id); err != nil {
			return err
		}

		if s.scheduleSvc != nil {
			if _, err := s.scheduleSvc.CancelPendingSleeps(cleanupCtx, id); err != nil {
				return fmt.Errorf("cancel one-shot waits for session %d: %w", id, err)
			}
		}
	}

	for _, link := range links {
		if err := s.links.MakeStoppedLinkResumable(cleanupCtx, link.ChildID); err != nil {
			return fmt.Errorf("detach stopped subagent %d: %w", link.ChildID, err)
		}
	}

	return s.markStoppedTreeSessions(cleanupCtx, ids, sessionID, keepRootStopping)
}

// markStoppedTreeSessions commits the final stopped status of every parked
// descendant; an explicit root stays in its fence for the caller's terminal
// transaction.
//
//nolint:funcorder // completes the stop transition documented above.
func (s *svc) markStoppedTreeSessions(
	ctx context.Context,
	ids []int64,
	rootID int64,
	keepRootStopping bool,
) error {
	for _, id := range ids {
		if keepRootStopping && id == rootID {
			continue
		}

		if err := s.sessionStore.UpdateSessionStatus(ctx, id, sessionstore.SessionStatusStopped); err != nil {
			return fmt.Errorf("mark session %d stopped: %w", id, err)
		}
	}

	return nil
}

func (s *svc) Clear(ctx context.Context, sessionID int64) (int64, error) {
	return s.clear(ctx, sessionID, 0)
}

//nolint:funcorder // Clear's command variant shares one replacement transaction with Clear.
func (s *svc) clear(ctx context.Context, sessionID, inputID int64) (int64, error) {
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
	if roots, ok := s.sessionStore.(sessionstore.ManagerRootStore); ok && owner != "" {
		if inputID > 0 {
			newRec, _, err = roots.ReplaceManagerRootForInput(ctx, sessionID, inputID, projectName, workDir)
		} else {
			newRec, _, err = roots.ReplaceManagerRoot(ctx, sessionID, projectName, workDir)
		}

		if err != nil {
			return 0, fmt.Errorf("replace manager session: %w", err)
		}
	} else {
		if err := s.sessionStore.UpdateSessionStatus(
			ctx,
			sessionID,
			sessionstore.SessionStatusTerminating,
		); err != nil {
			log.Warn("clear_set_terminating_failed", zap.Int64("session_id", sessionID), zap.Error(err))
		}

		newRec, err = s.sessionStore.CreateSession(ctx, rec.ProjectID, rec.Model, rec.ReasoningLevel, rec.Attributes)
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

	if err := s.Kill(ctx, sessionID); err != nil {
		log.Warn("clear_kill_old_session_failed", zap.Int64("session_id", sessionID), zap.Error(err))
	}

	return newRec.ID, nil
}

// SetModel applies the switch before recording it: a model the session cannot
// run must never land in the record, or the session stops being resumable.
func (s *svc) SetModel(ctx context.Context, sessionID int64, model, reasoningLevel string) error {
	if err := s.checkModelConfigured(model); err != nil {
		return err
	}

	if budgets, ok := s.sessionStore.(sessionstore.BudgetStore); ok {
		record, loadErr := s.sessionStore.GetSession(ctx, sessionID)
		if loadErr != nil {
			return fmt.Errorf("load session for budgeted model switch: %w", loadErr)
		}

		budgetRecord, budgetErr := budgets.GetBudget(ctx, sessionRootID(record))
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

	s.mu.Lock()
	rs, ok := s.loops[sessionID]
	s.mu.Unlock()

	var sessSvc session.Service

	if ok {
		rs.svcMu.Lock()
		sessSvc = rs.service
		rs.svcMu.Unlock()
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

//nolint:wsl_v5 // Cancellation sources are adjacent before joins begin.
func (s *svc) Shutdown(timeout time.Duration) {
	s.shuttingDown.Store(true)

	if s.progressCancel != nil {
		s.progressCancel()
	}
	if s.budgetCancel != nil {
		s.budgetCancel()
	}

	recoveryDone := s.stopRecovery()

	s.mu.Lock()
	runners := make([]*runner, 0, len(s.loops))

	for _, rs := range s.loops {
		runners = append(runners, rs)
	}

	s.mu.Unlock()

	done := make(chan struct{})

	go func() {
		var wg sync.WaitGroup

		wg.Add(len(runners))

		for _, rs := range runners {
			go func(r *runner) {
				defer wg.Done()

				r.stop()
			}(rs)
		}

		wg.Wait()

		if recoveryDone != nil {
			<-recoveryDone
		}

		if s.progressDone != nil {
			<-s.progressDone
		}

		s.budgetWG.Wait()

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
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

func (s *svc) GetOrCreateSystemProject(ctx context.Context, workDir, name string) (int64, error) {
	if !sameProjectPath(workDir, s.systemProject) {
		return 0, errors.New("system project is outside the canonical configuration directory")
	}

	id, err := s.store.GetOrCreateSystemProject(ctx, workDir, name)
	if err != nil {
		return 0, fmt.Errorf("resolve system project: %w", err)
	}

	return id, nil
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

func (s *svc) enqueueUserSessionInput(
	ctx context.Context,
	sessionID int64,
	prompt string,
) (*sessionstore.InboxInput, error) {
	modelInputs, ok := s.inboxStore.(sessionstore.ModelInputStore)
	if !ok || isExactControlCommand(prompt) {
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

	input, err := modelInputs.EnqueueModelInput(ctx, sessionID, prompt)
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
	s.recoveryMu.Lock()
	cancel := s.recoveryCancel
	done := s.recoveryDone
	s.recoveryMu.Unlock()

	if cancel != nil {
		cancel()
	}

	return done
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
// manager lock, returning true if a live runner existed. Holding the lock across
// the append serializes it with runner teardown (delete + leftover drain).
func (s *svc) appendIfLive(sessionID int64, input queuedSessionInput) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if rs, ok := s.loops[sessionID]; ok {
		rs.appendSessionInput(input)
		return true
	}

	return false
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

	// Append under s.mu so the check-and-append is atomic against the runner's
	// teardown (which deletes from s.loops under the same lock, then drains
	// leftover inputs) — otherwise a completion racing an exiting suspended
	// parent could be lost.
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

	return s.ensureRunner(ctx, sessionID, workDir, rec.ProjectID, []queuedSessionInput{input})
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

	if roots, ok := s.sessionStore.(sessionstore.ManagerRootStore); ok && owner != "" {
		projectName, nameErr := s.store.GetProjectName(ctx, projectID)
		if nameErr != nil {
			return 0, fmt.Errorf("resolve project name: %w", nameErr)
		}

		rec, _, err = roots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
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

		return 0, err
	}

	return rec.ID, nil
}
