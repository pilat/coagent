package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/transcript"
)

const assistantRole = "assistant"

const (
	// A child killed with a non-terminal link is invisible to the sweep forever,
	// so the terminal mark is worth retrying rather than logging once.
	linkTerminalAttempts = 3
	linkTerminalBackoff  = 150 * time.Millisecond

	// cascadeRetryBudget caps retries across a WHOLE cascade kill, not per node:
	// the walk is sequential and sits on the synchronous Kill path, so a per-node
	// budget multiplies by the size of the tree.
	cascadeRetryBudget = 2 * time.Second
)

func (s *svc) deriveOutcome(
	ctx context.Context,
	childID int64,
	iterations int,
	errored bool,
	persistedError bool,
	emptyStopStreak int,
) (string, subagent.Outcome) {
	// A durable terminal empty streak is recovery evidence, not an error: the
	// shared host notice is the child's successful result even when the daemon
	// restarts before link terminalization.
	if emptyStopStreak >= sessionstore.EmptyStopTerminalStreak {
		return sessionstore.EmptyStopTerminalNotice(emptyStopStreak), subagent.OutcomeCompleted
	}

	if persistedError {
		if result, outcome, ok := s.currentIntegrityOutcome(ctx, childID); ok {
			return result, outcome
		}
	}

	messages, err := s.store.LoadActiveMessages(ctx, childID)
	if err != nil {
		// A load failure is not an empty transcript: nil messages would
		// masquerade as "no final answer" below, so report the error.
		logger.Ctx(ctx).Named("daemon.completion").Error(
			"load_active_messages", zap.Int64("child", childID), zap.Error(err),
		)

		return fmt.Sprintf("could not load final messages after %d iterations", iterations),
			subagent.OutcomeError
	}

	finalText := lastAssistantText(messages)
	switch {
	case persistedError && lastAssistantHasCalls(messages):
		return fmt.Sprintf("ended without a final answer after %d iterations", iterations),
			subagent.OutcomeIncomplete
	case persistedError || errored:
		return fmt.Sprintf("crashed after %d iterations", iterations), subagent.OutcomeError
	case lastMessageIsFinalAnswer(messages):
		// A confirmed completion check leaves a durable pointer at the
		// candidate row: the child's answer is that full text, not the
		// trailing ack. Error and terminal-empty outcomes above keep
		// precedence; without the pointer the final text stands (a
		// background-yield child).
		if record, recordErr := s.store.GetSession(ctx, childID); recordErr == nil &&
			record.CompletionCheckConfirmedAnswerID != nil {
			if answer, answerErr := s.store.LoadMessageContentByID(
				ctx, childID, *record.CompletionCheckConfirmedAnswerID,
			); answerErr == nil && strings.TrimSpace(answer) != "" {
				return answer, subagent.OutcomeCompleted
			}
		}

		return finalText, subagent.OutcomeCompleted
	default:
		return fmt.Sprintf("ended without a final answer after %d iterations", iterations),
			subagent.OutcomeIncomplete
	}
}

func (s *svc) currentIntegrityOutcome(
	ctx context.Context,
	childID int64,
) (string, subagent.Outcome, bool) {
	rejection, err := s.store.LoadCurrentTerminalRejection(ctx, childID)
	if err == nil {
		switch rejection.RejectedReason {
		case sessionstore.RejectedReasonOutputLength:
			return sessionstore.OutputLengthTerminalError, subagent.OutcomeError, true
		case sessionstore.RejectedReasonUnknownFinish:
			return sessionstore.UnknownFinishTerminalError, subagent.OutcomeError, true
		}
	}

	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false
	}

	logger.Ctx(ctx).Named("daemon.completion").Error(
		"load_terminal_rejection", zap.Int64("child", childID), zap.Error(err),
	)

	if err != nil {
		return "protocol inconsistency while loading the current session error", subagent.OutcomeError, true
	}

	return "", "", false
}

func lastAssistantHasCalls(messages []*transcript.Message) bool {
	for _, message := range slices.Backward(messages) {
		if message.Role == assistantRole {
			return len(message.ToolCalls) > 0
		}

		if message.Role == "user" {
			return false
		}
	}

	return false
}

func lastAssistantText(messages []*transcript.Message) string {
	for _, message := range slices.Backward(messages) {
		if isFinalAssistantMessage(message) {
			return message.Content
		}
	}

	return ""
}

func lastMessageIsFinalAnswer(messages []*transcript.Message) bool {
	for _, message := range slices.Backward(messages) {
		switch message.Role {
		case assistantRole:
			return isFinalAssistantMessage(message)
		case "user":
			return false
		}
	}

	return false
}

func isFinalAssistantMessage(message *transcript.Message) bool {
	return message.Role == assistantRole && len(message.ToolCalls) == 0 &&
		strings.TrimSpace(message.Content) != "" &&
		(message.FinishType == "" || message.FinishType == "stop")
}

// markLinkTerminalRetrying retries MarkLinkTerminal until it succeeds, runs out of
// attempts, or passes deadline (zero deadline = attempts only). The pause is
// uninterruptible: callers pass a WithoutCancel ctx, so Done() never fires.
func (s *svc) markLinkTerminalRetrying(
	ctx context.Context,
	deadline time.Time,
	childID int64,
	state subagent.State,
	result string,
	outcome subagent.Outcome,
) error {
	var err error

	for attempt := range linkTerminalAttempts {
		if attempt > 0 && !deadline.IsZero() && time.Now().After(deadline) {
			return err
		}

		if attempt > 0 {
			time.Sleep(linkTerminalBackoff)
		}

		err = s.links.MarkLinkTerminal(ctx, childID, state, result, outcome)
		if err == nil {
			return nil
		}
	}

	return fmt.Errorf("mark link terminal for child %d: %w", childID, err)
}

// notifyChildFailure reports on the PARENT's topic: a child that never reached
// announceSession has no topic of its own, so publishing to it reaches nobody.
func (s *svc) notifyChildFailure(ctx context.Context, parentID, childID int64, what string, err error) {
	if parentID == 0 {
		return
	}

	message := fmt.Sprintf("⚠️ Subagent %d: %s — %s", childID, what, logger.Redact(err.Error()))
	if outputErr := s.enqueueChildFailureOutput(ctx, parentID, childID, message); outputErr != nil {
		logger.Named("daemon.finalize").Warn("enqueue_child_failure_output", zap.Error(outputErr))
	}

	s.publish(parentID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: message})
}

func (s *svc) enqueueChildFailureOutput(ctx context.Context, parentID, childID int64, message string) error {
	outputs := s.store
	if outputs == nil {
		return nil
	}

	link, err := s.links.GetLink(ctx, childID)
	if err != nil || link == nil || link.ParentID != parentID || link.ActivationSeq <= 0 {
		return s.enqueuePersistentOutput(ctx, parentID, message)
	}

	attributes := map[string]any{"source": "agent"}

	_, err = outputs.EnqueueOutput(ctx, sessionstore.OutputDraft{
		SessionID:  parentID,
		Type:       sessionstore.OutputMessagePersistent,
		Content:    message,
		Attributes: attributes,
		SourceKey:  fmt.Sprintf("child:%d:%d:outcome", childID, link.ActivationSeq),
		Fingerprint: sessionstore.OutputFingerprint(
			sessionstore.OutputMessagePersistent,
			message,
			parentID,
			attributes,
		),
	})
	if errors.Is(err, sessionstore.ErrOutputOwner) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("enqueue child failure output: %w", err)
	}

	return nil
}
