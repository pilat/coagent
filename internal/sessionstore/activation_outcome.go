package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/transcript"
)

const (
	ActivationCompleted  ActivationOutcomeKind = "completed"
	ActivationFailed     ActivationOutcomeKind = "error"
	ActivationIncomplete ActivationOutcomeKind = "incomplete"
)

// ActivationOutcomeKind identifies the canonical recovered result of an activation.
type ActivationOutcomeKind string

// ActivationOutcome projects existing session and transcript evidence without a second ledger.
type ActivationOutcome struct {
	Kind       ActivationOutcomeKind
	Text       string
	Status     SessionStatus
	Diagnostic error
}

// LoadActivationOutcome preserves durable terminal evidence before interpreting transcript fallback.
func (s *store) LoadActivationOutcome(ctx context.Context, sessionID int64, errored bool) (*ActivationOutcome, error) {
	record, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load activation session: %w", err)
	}

	status := SessionStatusCompleted
	if (errored || record.Status == SessionStatusError) && record.EmptyStopStreak < EmptyStopTerminalStreak {
		status = SessionStatusError
	}

	text, kind, diagnostic := s.deriveOutcome(ctx, record, errored)

	return &ActivationOutcome{Kind: kind, Text: text, Status: status, Diagnostic: diagnostic}, nil
}

func (s *store) deriveOutcome(
	ctx context.Context,
	record *SessionRecord,
	errored bool,
) (string, ActivationOutcomeKind, error) {
	childID, iterations := record.ID, record.Iteration

	persistedError, emptyStopStreak := record.Status == SessionStatusError, record.EmptyStopStreak
	if emptyStopStreak >= EmptyStopTerminalStreak {
		return EmptyStopTerminalNotice(emptyStopStreak), ActivationCompleted, nil
	}

	if persistedError {
		if result, outcome, ok, diagnostic := s.currentIntegrityOutcome(ctx, childID); ok {
			return result, outcome, diagnostic
		}
	}

	messages, err := s.LoadActiveMessages(ctx, childID)
	if err != nil {
		return fmt.Sprintf("could not load final messages after %d iterations", iterations),
			ActivationFailed, err
	}

	finalText := lastAssistantText(messages)
	switch {
	case persistedError && lastAssistantHasCalls(messages):
		return fmt.Sprintf("ended without a final answer after %d iterations", iterations),
			ActivationIncomplete, nil
	case persistedError || errored:
		return fmt.Sprintf("crashed after %d iterations", iterations), ActivationFailed, nil
	case lastMessageIsFinalAnswer(messages):
		if record.CompletionCheckConfirmedAnswerID != nil {
			if answer, answerErr := s.LoadMessageContentByID(
				ctx, childID, *record.CompletionCheckConfirmedAnswerID,
			); answerErr == nil && strings.TrimSpace(answer) != "" {
				return answer, ActivationCompleted, nil
			}
		}

		return finalText, ActivationCompleted, nil
	default:
		return fmt.Sprintf("ended without a final answer after %d iterations", iterations),
			ActivationIncomplete, nil
	}
}

func (s *store) currentIntegrityOutcome(
	ctx context.Context,
	childID int64,
) (string, ActivationOutcomeKind, bool, error) {
	rejection, err := s.LoadCurrentTerminalRejection(ctx, childID)
	if err == nil {
		switch rejection.RejectedReason {
		case RejectedReasonOutputLength:
			return OutputLengthTerminalError, ActivationFailed, true, nil
		case RejectedReasonUnknownFinish:
			return UnknownFinishTerminalError, ActivationFailed, true, nil
		}
	}

	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}

	if err != nil {
		return "protocol inconsistency while loading the current session error", ActivationFailed, true, err
	}

	return "", "", false, nil
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
