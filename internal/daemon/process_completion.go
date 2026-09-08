package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
)

const (
	processRetryMaxExponent = 5
	processRetryDelay       = 200 * time.Millisecond
)

type processRetryState struct {
	attempts  int
	scheduled bool
}

// processDeliverySink abstracts the manager surface the coordinator needs,
// keeping the coordinator unit-testable without the full manager.
type processDeliverySink interface {
	withProcessDeliveryFence(ctx context.Context, rootID int64, run func() error) error
	enqueueProcessCompletionLocked(
		ctx context.Context,
		target int64,
		input processCompletionInput,
	) error
}

// processCompletionInput wakes a session with one durable terminal process
// fact. The completion is already claimed in the ledger; the injection is
// exactly-once through the session delivery identity.
type processCompletionInput struct {
	Completion backgroundprocess.Completion
}

func (processCompletionInput) isSessionInput() {}

func (i processCompletionInput) validate() error {
	if i.Completion.ProcessID == "" {
		return errors.New("process completion requires a process id")
	}

	if i.Completion.SessionID <= 0 || i.Completion.RootID <= 0 {
		return errors.New("process completion requires positive origin ids")
	}

	return nil
}

// processCoordinator converts durable terminal process facts into owner or
// root session events. Routing and target selection commit in one claimed
// transaction; delivery is exactly-once via the claimed delivery state and
// the session delivery identity.
type processCoordinator struct {
	store backgroundprocess.Store
	sink  processDeliverySink
}

func newProcessCoordinator(
	store backgroundprocess.Store,
	sink processDeliverySink,
) *processCoordinator {
	return &processCoordinator{store: store, sink: sink}
}

// Route claims the completion delivery target for a terminal process and
// queues the wake input. Target selection and the claim commit in one
// transaction, so a subagent finishing concurrently cannot split routing.
// Idempotent: only the first claim wins; a lost claim means another caller
// owns delivery.
func (c *processCoordinator) Route(
	ctx context.Context,
	completion backgroundprocess.Completion,
) (int64, bool, error) {
	var target int64
	var claimed bool

	err := c.sink.withProcessDeliveryFence(ctx, completion.RootID, func() error {
		var err error

		target, claimed, err = c.store.ClaimDelivery(ctx, completion.ProcessID)
		if err != nil {
			return fmt.Errorf("claim process delivery: %w", err)
		}

		if !claimed {
			return nil
		}

		err = c.sink.enqueueProcessCompletionLocked(
			ctx, target, processCompletionInput{Completion: completion},
		)

		return err
	})

	return target, claimed, err
}

// RouteRestarted re-delivers a completion whose routing survived a restart.
// The claim is already held; the enqueue is at-least-once at the producer
// boundary while the session delivery identity keeps the transcript
// exactly-once. The preview is re-extracted from the output file, so a
// restart does not degrade a readable tail into an omitted-binary lie.
func (c *processCoordinator) RouteRestarted(
	ctx context.Context,
	record backgroundprocess.Process,
	target int64,
) error {
	return c.sink.withProcessDeliveryFence(ctx, record.RootSessionID, func() error {
		return c.sink.enqueueProcessCompletionLocked(
			ctx, target, processCompletionInput{Completion: restartCompletion(record)},
		)
	})
}

// restartCompletion rebuilds the bounded completion fact from the durable
// record, re-reading the tail preview from the preserved output file.
func restartCompletion(record backgroundprocess.Process) backgroundprocess.Completion {
	completion := backgroundprocess.Completion{
		ProcessID:  record.ID,
		SessionID:  record.SessionID,
		RootID:     record.RootSessionID,
		ToolCallID: record.ToolCallID,
		State:      record.State,
		OutputPath: record.OutputPath,
		OutputSize: record.OutputSize,
	}

	if record.ExitCode != nil {
		completion.ExitCode = *record.ExitCode
		completion.HasExitCode = true
	}

	if record.FinishedAt != nil {
		completion.Duration = record.FinishedAt.Sub(record.CreatedAt)
	}

	tail, truncated, ok := backgroundprocess.ExtractTail(
		record.OutputPath, backgroundprocess.TailPreviewLines, backgroundprocess.TailPreviewBytes,
	)

	binary := !ok || !backgroundprocess.ValidEventTail(tail)
	completion.BinaryTail = binary
	completion.TailOmitted = binary

	if !binary {
		completion.Tail = tail
		if truncated {
			completion.Tail = backgroundprocess.TruncateTailToBytes(
				tail, backgroundprocess.TailPreviewBytes,
			)
		}
	}

	return completion
}

func (s *svc) withProcessDeliveryFence(ctx context.Context, rootID int64, run func() error) error {
	unlock, err := s.lockSessionTree(ctx, rootID)
	if err != nil {
		return err
	}
	defer unlock()

	return run()
}

func (s *svc) enqueueProcessCompletionLocked(
	ctx context.Context,
	target int64,
	input processCompletionInput,
) error {
	if err := input.validate(); err != nil {
		return err
	}

	err := s.routeQueuedSessionInputLocked(ctx, target, asyncSessionInput{value: input})
	if !errors.Is(err, admission.ErrNoCapacity) {
		return err
	}

	return s.queueCapacityBlockedProcessTargetLocked(ctx, target)
}

func (s *svc) queueCapacityBlockedProcessTargetLocked(ctx context.Context, target int64) error {
	record, loadErr := s.sessionStore.GetSession(ctx, target)
	if loadErr != nil {
		return fmt.Errorf("load capacity-blocked process target %d: %w", target, loadErr)
	}

	workDir, loadErr := s.store.GetProjectWorkDir(ctx, record.ProjectID)
	if loadErr != nil {
		return fmt.Errorf("resolve capacity-blocked process target %d: %w", target, loadErr)
	}

	s.enqueuePendingRunner(target, workDir, record.ProjectID)
	go s.drainPendingRunners(context.WithoutCancel(ctx))

	return nil
}

// injectProcessCompletion runs on the target session's runner: build the
// bounded synthetic pair and persist it exactly once.
func (s *svc) injectProcessCompletion(
	ctx context.Context,
	sess session.Service,
	targetSessionID int64,
	completion backgroundprocess.Completion,
) (bool, error) {
	event := session.ProcessEvent{
		ProcessID:       completion.ProcessID,
		OriginSessionID: completion.SessionID,
		State:           string(completion.State),
		ExitCode:        completion.ExitCode,
		HasExitCode:     completion.HasExitCode,
		Duration:        completion.Duration,
		OutputPath:      completion.OutputPath,
		Tail:            completion.Tail,
		TailOmitted:     completion.TailOmitted,
	}

	if targetSessionID == completion.RootID && completion.SessionID != completion.RootID {
		event.OriginSubagent = strconv.FormatInt(completion.SessionID, 10)
	}

	applied, err := sess.InjectProcessCompletion(ctx, completion.ProcessID, event)
	if err != nil {
		return false, fmt.Errorf("inject process event %s: %w", completion.ProcessID, err)
	}

	if _, err := s.processStore.MarkDelivered(ctx, completion.ProcessID); err != nil {
		logger.Ctx(ctx).Named("daemon.process").Warn(
			"process_mark_delivered_failed",
			zap.String("process", completion.ProcessID), zap.Error(err),
		)
		s.scheduleProcessRetry(
			context.WithoutCancel(ctx), targetSessionID, completion,
		)

		return applied, nil
	}

	s.clearProcessRetry(completion.ProcessID)

	return applied, nil
}

func (s *svc) scheduleProcessRetry(
	ctx context.Context,
	targetSessionID int64,
	completion backgroundprocess.Completion,
) {
	delay, reserved := s.reserveProcessRetry(completion.ProcessID)
	if !reserved {
		return
	}

	go func() {
		defer s.workerWG.Done()

		retryCtx, cancel := s.newDaemonWorkerContext(ctx)
		defer cancel()

		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-retryCtx.Done():
			s.releaseProcessRetry(completion.ProcessID)

			return
		}

		s.releaseProcessRetry(completion.ProcessID)

		if s.shuttingDown.Load() {
			return
		}

		record, err := s.processStore.GetProcess(retryCtx, completion.ProcessID)
		if err != nil {
			logger.Ctx(ctx).Named("daemon.process").Warn(
				"process_retry_load_failed", zap.String("process", completion.ProcessID), zap.Error(err),
			)
			s.scheduleProcessRetry(ctx, targetSessionID, completion)

			return
		}

		if record.DeliveryState == "delivered" || record.DeliveryState == "suppressed" {
			s.clearProcessRetry(record.ID)

			return
		}

		if record.DeliveryState == "claimed" {
			targetSessionID = record.DeliveryTargetSessionID
		}

		if targetSessionID == 0 {
			target, claimed, routeErr := s.processCoord.Route(
				retryCtx, restartCompletion(record),
			)
			if routeErr != nil {
				s.logProcessRouteFailure(ctx, record.ID, routeErr)
			}

			if claimed || routeErr != nil {
				targetSessionID = target
			}

			s.scheduleProcessRetry(ctx, targetSessionID, completion)

			return
		}

		if routeErr := s.processCoord.RouteRestarted(
			retryCtx, record, targetSessionID,
		); routeErr != nil {
			s.logProcessRouteFailure(ctx, record.ID, routeErr)
		}

		s.scheduleProcessRetry(ctx, targetSessionID, completion)
	}()
}

func (s *svc) reserveProcessRetry(processID string) (time.Duration, bool) {
	s.processRetryMu.Lock()
	defer s.processRetryMu.Unlock()

	if s.shuttingDown.Load() {
		return 0, false
	}

	state := s.processRetries[processID]
	if state.scheduled {
		return 0, false
	}

	exponent := min(state.attempts, processRetryMaxExponent)
	state.attempts++
	state.scheduled = true
	s.processRetries[processID] = state
	s.workerWG.Add(1)

	return processRetryDelay << exponent, true
}

func (s *svc) newDaemonWorkerContext(ctx context.Context) (context.Context, context.CancelFunc) {
	retryCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(s.workerCtx, cancel)

	return retryCtx, func() {
		stop()
		cancel()
	}
}

func (s *svc) releaseProcessRetry(processID string) {
	s.processRetryMu.Lock()
	defer s.processRetryMu.Unlock()

	state, ok := s.processRetries[processID]
	if !ok {
		return
	}

	state.scheduled = false
	s.processRetries[processID] = state
}

func (s *svc) clearProcessRetry(processID string) {
	s.processRetryMu.Lock()
	defer s.processRetryMu.Unlock()

	delete(s.processRetries, processID)
}

func (s *svc) routeProcessCompletion(
	ctx context.Context,
	completion backgroundprocess.Completion,
) {
	target, _, err := s.processCoord.Route(ctx, completion)
	if err != nil {
		s.logProcessRouteFailure(ctx, completion.ProcessID, err)
	}

	s.scheduleProcessRetry(context.WithoutCancel(ctx), target, completion)
}

func (s *svc) routeRestartedProcessCompletion(
	ctx context.Context,
	record backgroundprocess.Process,
	target int64,
) {
	err := s.processCoord.RouteRestarted(ctx, record, target)
	if err != nil {
		s.logProcessRouteFailure(ctx, record.ID, err)
	}

	s.scheduleProcessRetry(context.WithoutCancel(ctx), target, restartCompletion(record))
}

func (s *svc) logProcessRouteFailure(ctx context.Context, processID string, err error) {
	logger.Ctx(ctx).Named("daemon.process").Error(
		"process_route_failed", zap.String("process", processID), zap.Error(err),
	)
}
