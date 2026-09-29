package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

const compactionAttemptCap = 3

var _ contextRuntime = (*checkpointOwner)(nil)

type contextRuntime interface {
	request()
	requested() bool
	beginRun()
	deferred() bool
	queueCommand(context.Context, PendingInput, string, func(context.Context, string) error) (boundaryOutcome, error)
	apply(context.Context, func(context.Context, string) error) checkpointResult
	resetOnce(context.Context, string, string) (bool, error)
	projectContextSize() (int, bool)
}

type checkpointCalls interface {
	PendingExternalCalls() []PendingToolCall
	HasPendingWork() bool
	PendingExternalCallIDs([]llmwire.Message) map[string]bool
}

type completionReader interface {
	LoadCompletionCheckState(context.Context, int64) (*sessionstore.CompletionCheckState, error)
}

type toolSchemaPolicy interface {
	schemas() []llmwire.ToolSchema
	inventorySchemas() []llmwire.ToolSchema
}

type checkpointOptions struct {
	id             int64
	outputEnabled  bool
	agentsMD       string
	deferAnnounced bool
}

type checkpointResult struct {
	committed   bool
	budgetFired bool
}

type checkpointOwner struct {
	ms                       *messageStore
	models                   modelRuntime
	prompt                   *promptBuilder
	tools                    toolSchemaPolicy
	calls                    checkpointCalls
	dispositions             completionReader
	budgetGate               BudgetGate
	outputStore              sessionstore.RuntimeOutputStore
	boundary                 InputBoundary
	id                       int64
	outputEnabled            bool
	agentsMD                 string
	stamper                  *timestamper
	activeSubagentsProvider  func(context.Context) []ActiveSubagentInfo
	activeProcessesProvider  func(context.Context) []ActiveProcessInfo
	control                  checkpointControl
	compactionSummaryDBID    int64
	compactionDeferAnnounced bool
	compactionFailures       int
	autoCompactionOff        bool
	notifyFn                 func(context.Context, string) error
}

func newCheckpointOwner(
	ms *messageStore,
	models modelRuntime,
	prompt *promptBuilder,
	tools toolSchemaPolicy,
	calls checkpointCalls,
	dispositions completionReader,
	budgetGate BudgetGate,
	outputStore sessionstore.RuntimeOutputStore,
	boundary InputBoundary,
	stamper *timestamper,
	activeSubagentsProvider func(context.Context) []ActiveSubagentInfo,
	activeProcessesProvider func(context.Context) []ActiveProcessInfo,
	options checkpointOptions,
) contextRuntime {
	return &checkpointOwner{
		ms: ms, models: models, prompt: prompt, tools: tools, calls: calls,
		dispositions: dispositions, budgetGate: budgetGate, outputStore: outputStore,
		boundary: boundary, id: options.id, outputEnabled: options.outputEnabled,
		agentsMD: options.agentsMD, stamper: stamper,
		activeSubagentsProvider: activeSubagentsProvider, activeProcessesProvider: activeProcessesProvider,
		compactionDeferAnnounced: options.deferAnnounced,
	}
}

func (s *checkpointOwner) beginRun() {
	s.compactionFailures = 0

	s.autoCompactionOff = false
	if len(s.calls.PendingExternalCalls()) == 0 {
		s.compactionDeferAnnounced = false
	}
}

func (s *checkpointOwner) deferred() bool { return s.compactionDeferAnnounced }

func (s *checkpointOwner) request() {
	s.control.request()
}

func (s *checkpointOwner) requested() bool {
	return s.control.requested()
}

func (s *checkpointOwner) queueCommand(
	ctx context.Context,
	input PendingInput,
	focus string,
	notify func(context.Context, string) error,
) (boundaryOutcome, error) {
	s.notifyFn = notify
	if len(s.calls.PendingExternalCalls()) > 0 {
		if !s.compactionDeferAnnounced {
			s.compactionDeferAnnounced = true
			if err := s.enqueueCompactionNotice(ctx, input, "deferred",
				sessionstore.OutputMessagePersistent, compactionDeferredNotice); err != nil {
				return commandNotRecognized, err
			}

			s.notify(ctx, compactionDeferredNotice)
		}

		return commandDeferred, nil
	}

	s.control.queue(input, strings.TrimSpace(focus))

	return commandDeferred, nil
}

func (s *checkpointOwner) resetOnce(ctx context.Context, deliveryID, prompt string) (bool, error) {
	if deliveryID == "" {
		return false, errors.New("idempotent context reset: empty delivery id")
	}

	if pending := s.calls.PendingExternalCalls(); len(pending) > 0 {
		return false, fmt.Errorf(
			"reset context: external call %s (%s) is still pending",
			pending[0].ID,
			pending[0].Name,
		)
	}

	opening := openingTurn(s.agentsMD, s.stamper, prompt)
	fingerprint := deliveryFingerprint("context_reset", s.agentsMD, prompt)

	inserted, err := s.ms.resetToOnce(ctx, deliveryID, fingerprint, opening)
	if err != nil {
		return false, fmt.Errorf("reset transcript: %w", err)
	}

	if inserted {
		s.models.clearBaseline(ctx)
	}

	return inserted, nil
}

func (s *checkpointOwner) notify(ctx context.Context, content string) {
	if s.notifyFn != nil {
		if err := s.notifyFn(ctx, content); err != nil {
			logger.Ctx(ctx).Named("session.compaction").Warn("notify_failed", zap.Error(err))
		}
	}
}

func (s *checkpointOwner) notifyPersistent(ctx context.Context, content string) {
	if s.outputEnabled {
		if err := s.ms.enqueueOutput(ctx, sessionstore.OutputMessagePersistent, content); err != nil {
			logger.Ctx(ctx).Named("session.compaction").Warn("enqueue_output_failed", zap.Error(err))
		}
	}

	s.notify(ctx, content)
}

func (s *checkpointOwner) handleCommandOutput(ctx context.Context, input PendingInput, reason, output string) error {
	if boundary, ok := s.boundary.(outputCommandBoundary); ok {
		if err := boundary.HandleWithOutput(ctx, input, reason, output); err != nil {
			return fmt.Errorf("resolve %s: %w", reason, err)
		}

		return nil
	}

	if err := s.boundary.Handle(ctx, input, reason); err != nil {
		return fmt.Errorf("resolve %s: %w", reason, err)
	}

	if s.outputEnabled {
		return s.ms.enqueueOutput(ctx, sessionstore.OutputMessagePersistent, output)
	}

	return nil
}
