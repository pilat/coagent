package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

const assistantFailed = "failed"

func (s *svc) observeAssistantWork(ctx context.Context, sessionID int64, runErr error) error {
	sends, err := s.assistantStore.UnobservedSends(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list assistant observations: %w", err)
	}

	if len(sends) == 0 {
		return nil
	}

	// The runtime's own yield verdict: a final written while this is true waits for that work.
	busy, err := s.store.HasBackgroundWakeSource(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("check assistant work wake source: %w", err)
	}

	for _, send := range sends {
		if err := s.observeAssistantSend(ctx, send, busy, runErr); err != nil {
			return err
		}
	}

	return nil
}

func (s *svc) observeAssistantSend(ctx context.Context, send assistant.Send, busy bool, runErr error) error {
	record, err := s.store.GetSession(ctx, send.SessionID)
	if err != nil {
		return fmt.Errorf("load observed work session: %w", err)
	}

	project, err := s.store.GetProjectName(ctx, record.ProjectID)
	if err != nil {
		return fmt.Errorf("load observed project: %w", err)
	}

	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		view, err := sessionstore.ObserveAssistantInputTx(ctx, tx, send.InputID)
		if err != nil {
			return fmt.Errorf("observe assistant input: %w", err)
		}

		kind, text, settled := assistantObservation(view, busy, runErr)
		if !settled {
			return nil
		}

		won, err := assistant.MarkObservedTx(ctx, tx, send.InputID, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("settle assistant observation: %w", err)
		}

		if !won {
			return nil
		}

		if kind != "" {
			if kind == "answered" {
				text = fmt.Sprintf("%q", text)
			}

			body := fmt.Sprintf("← %s (%d) %s: %s", project, send.SessionID, kind, text)
			in := assistantEventInput(send.AssistantSessionID, send.SessionID, kind, body)
			in.DeliveryKey = fmt.Sprintf("assistant:send:%d", send.InputID)

			if _, err := sessionstore.EnqueueTx(ctx, tx, in); err != nil {
				return fmt.Errorf("enqueue assistant observation: %w", err)
			}
		}

		return nil
	})
	if errors.Is(err, sessionstore.ErrSessionNotAcceptingInput) {
		logger.Ctx(ctx).Named("daemon.assistant").Warn("assistant observation deferred", zap.Error(err))
		return nil
	}

	if err != nil {
		return fmt.Errorf("commit assistant observation: %w", err)
	}

	return nil
}

func (s *svc) sweepAssistantObservations(ctx context.Context) error {
	sends, err := s.assistantStore.UnobservedSends(ctx, 0)
	if err != nil {
		return fmt.Errorf("load assistant recovery obligations: %w", err)
	}

	seen := make(map[int64]bool)
	for _, send := range sends {
		if seen[send.SessionID] {
			continue
		}

		seen[send.SessionID] = true

		unlock, err := s.lockSessionTree(ctx, send.SessionID)
		if err != nil {
			return err
		}

		if !s.HasActiveLoop(send.SessionID) {
			err = s.observeAssistantWork(ctx, send.SessionID, nil)
		}

		unlock()

		if err != nil {
			return err
		}
	}

	return nil
}

func assistantObservation(view *sessionstore.AssistantObservation, busy bool, runErr error) (string, string, bool) {
	input, record := view.Input, view.Session
	if input.State == sessionstore.InputStateCancelled || input.State == sessionstore.InputStateRejected {
		cause := string(sessionstore.SessionStatusStopped)
		if record.KilledAt != nil || strings.Contains(input.ResolutionReason, "kill") {
			cause = string(sessionstore.SessionStatusKilled)
		}

		return assistantFailed, cause, true
	}

	terminal := record.Status == sessionstore.SessionStatusStopped ||
		record.Status == sessionstore.SessionStatusKilled ||
		record.Status == sessionstore.SessionStatusError
	if input.State == sessionstore.InputStatePending && !terminal {
		return "", "", false
	}

	if input.State == sessionstore.InputStatePending || terminal {
		cause := assistantFailure(record.Status, runErr)
		if view.Answer != "" {
			cause += "\n\nBefore that it said:\n" + view.Answer
		}

		return assistantFailed, cause, true
	}

	if busy {
		return "", "", false
	}

	if view.Settled && view.Answer != "" {
		return "answered", view.Answer, true
	}

	if view.NextUser != 0 {
		return "", "", true
	}

	return "", "", false
}

func assistantFailure(status sessionstore.SessionStatus, runErr error) string {
	if status == sessionstore.SessionStatusStopped {
		return string(status)
	}

	if status == sessionstore.SessionStatusKilled {
		return string(status)
	}

	if runErr != nil {
		return logger.Redact(runErr.Error())
	}

	if status == sessionstore.SessionStatusError {
		return "session error"
	}

	return "ended without an answer"
}
