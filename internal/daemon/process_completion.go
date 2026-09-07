package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
)

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

// processDeliverySink abstracts the manager surface the coordinator needs,
// keeping the coordinator unit-testable without the full manager.
type processDeliverySink interface {
	deliverProcessCompletion(ctx context.Context, target int64, input processCompletionInput) error
}

// processCoordinator converts durable terminal process facts into owner or
// root session events. Routing is one claimed transaction on the process
// record; delivery is exactly-once via the claimed delivery state and the
// session delivery identity.
type processCoordinator struct {
	store      backgroundprocess.Store
	sink       processDeliverySink
	getSession sessionGetter
}

// sessionGetter loads one durable session record.
type sessionGetter func(ctx context.Context, id int64) (*sessionstore.SessionRecord, error)

func newProcessCoordinator(
	store backgroundprocess.Store,
	sink processDeliverySink,
	getSession sessionGetter,
) *processCoordinator {
	return &processCoordinator{store: store, sink: sink, getSession: getSession}
}

// Route claims the completion delivery target for a terminal process and
// queues the wake input. Idempotent: only the first claim wins, and a lost
// claim means another caller owns delivery.
func (c *processCoordinator) Route(ctx context.Context, completion backgroundprocess.Completion) {
	log := logger.Ctx(ctx).Named("daemon.process")

	target, err := c.selectTarget(ctx, completion.SessionID, completion.RootID, c.getSession)
	if err != nil {
		log.Error("process_route_target_failed",
			zap.String("process", completion.ProcessID), zap.Error(err))

		return
	}

	claimed, err := c.store.ClaimDelivery(ctx, completion.ProcessID, target)
	if err != nil {
		log.Error("process_route_claim_failed",
			zap.String("process", completion.ProcessID), zap.Error(err))

		return
	}

	if !claimed {
		log.Debug("process_route_already_claimed", zap.String("process", completion.ProcessID))

		return
	}

	if err := c.sink.deliverProcessCompletion(ctx, target, processCompletionInput{Completion: completion}); err != nil {
		log.Error("process_wake_enqueue_failed",
			zap.String("process", completion.ProcessID),
			zap.Int64("target", target),
			zap.Error(err),
		)
	}
}

// RouteRestarted re-delivers a completion whose routing survived a restart.
// The claim is already held; the enqueue is at-least-once at the producer
// boundary while the session delivery identity keeps the transcript
// exactly-once.
func (c *processCoordinator) RouteRestarted(ctx context.Context, record backgroundprocess.Process, target int64) {
	completion := backgroundprocess.Completion{
		ProcessID:   record.ID,
		SessionID:   record.SessionID,
		RootID:      record.RootSessionID,
		ToolCallID:  record.ToolCallID,
		State:       record.State,
		OutputPath:  record.OutputPath,
		OutputSize:  record.OutputSize,
		TailOmitted: true,
		BinaryTail:  true,
	}

	if record.ExitCode != nil {
		completion.ExitCode = *record.ExitCode
		completion.HasExitCode = true
	}

	if record.FinishedAt != nil {
		completion.Duration = record.FinishedAt.Sub(record.CreatedAt)
	}

	if err := c.sink.deliverProcessCompletion(ctx, target, processCompletionInput{Completion: completion}); err != nil {
		logger.Ctx(ctx).Named("daemon.process").Warn(
			"process_restart_enqueue_failed",
			zap.String("process", record.ID),
			zap.Int64("target", target),
			zap.Error(err),
		)
	}
}

// selectTarget resolves the owning session state at the durable delivery
// decision: an active or suspended subagent wakes itself; every other owner
// shape (terminal subagent, vanished row) wakes the root.
func (c *processCoordinator) selectTarget(
	ctx context.Context,
	ownerID, rootID int64,
	getSession func(ctx context.Context, id int64) (*sessionstore.SessionRecord, error),
) (int64, error) {
	if ownerID == rootID {
		return rootID, nil
	}

	rec, err := getSession(ctx, ownerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rootID, nil
		}

		return 0, fmt.Errorf("load owner session %d: %w", ownerID, err)
	}

	switch rec.Status {
	case sessionstore.SessionStatusActive, sessionstore.SessionStatusSuspended:
		return ownerID, nil
	case sessionstore.SessionStatusCompleted, sessionstore.SessionStatusError,
		sessionstore.SessionStatusStopping, sessionstore.SessionStatusStopped,
		sessionstore.SessionStatusTerminating, sessionstore.SessionStatusKilled:
		return rootID, nil
	default:
		return rootID, nil
	}
}

// deliverProcessCompletion is the manager-side sink: enqueue the wake input at
// the target session. The runner dispatch performs the transcript injection.
func (s *svc) deliverProcessCompletion(
	ctx context.Context,
	target int64,
	input processCompletionInput,
) error {
	return s.enqueueSessionInput(ctx, target, input)
}

// injectProcessCompletion runs on the target session's runner: build the
// bounded synthetic pair and persist it exactly once.
func (s *svc) injectProcessCompletion(
	ctx context.Context,
	sess session.Service,
	completion backgroundprocess.Completion,
) error {
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

	if completion.SessionID != completion.RootID {
		link, err := s.links.GetLink(ctx, completion.SessionID)
		if err == nil && link != nil {
			event.OriginSubagent = strconv.FormatInt(link.ChildID, 10)
		}
	}

	if _, err := sess.InjectProcessCompletion(ctx, completion.ProcessID, event); err != nil {
		return fmt.Errorf("inject process event %s: %w", completion.ProcessID, err)
	}

	return nil
}
