package sessionlifecycle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/transcript"
)

const assistantRole = "assistant"

func (c *completions) deriveOutcome(
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
		if result, outcome, ok := c.currentIntegrityOutcome(ctx, childID); ok {
			return result, outcome
		}
	}

	messages, err := c.sessions.LoadActiveMessages(ctx, childID)
	if err != nil {
		// A load failure is not an empty transcript: nil messages would
		// masquerade as "no final answer" below, so report the error.
		logger.Ctx(ctx).Named("sessionlifecycle.completion").Error(
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
		if record, recordErr := c.sessions.GetSession(ctx, childID); recordErr == nil &&
			record.CompletionCheckConfirmedAnswerID != nil {
			if answer, answerErr := c.sessions.LoadMessageContentByID(
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

func (c *completions) currentIntegrityOutcome(
	ctx context.Context,
	childID int64,
) (string, subagent.Outcome, bool) {
	rejection, err := c.sessions.LoadCurrentTerminalRejection(ctx, childID)
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

	logger.Ctx(ctx).Named("sessionlifecycle.completion").Error(
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
