package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/managerdelivery"
)

type outputQueue struct {
	controllerapi.OutputQueueController
	manager *Manager
}

func newOutputQueue(controller controllerapi.OutputQueueController, manager *Manager) *outputQueue {
	return &outputQueue{OutputQueueController: controller, manager: manager}
}

func (q *outputQueue) Claim(ctx context.Context) (*managerdelivery.Item, error) {
	claim, err := q.ClaimOutput(ctx)

	var retryPending *controllerapi.OutputRetryPendingError
	if errors.As(err, &retryPending) {
		return nil, &managerdelivery.RetryPendingError{NextAt: retryPending.NextAt}
	}

	if errors.Is(err, controllerapi.ErrNoOutput) {
		return nil, managerdelivery.ErrNoItem
	}

	if err != nil {
		return nil, fmt.Errorf("claim cli output: %w", err)
	}

	return &managerdelivery.Item{
		ID: claim.ID, AttemptID: claim.AttemptID, Attempts: claim.AttemptSeq, Payload: claim,
	}, nil
}

func (q *outputQueue) Ack(ctx context.Context, item *managerdelivery.Item, result managerdelivery.Result) error {
	if err := q.AckOutput(
		ctx,
		controllerapi.OutputAckData{ID: item.ID, AttemptID: item.AttemptID, MessageIDs: []string{}},
	); err != nil {
		return fmt.Errorf("ack cli output: %w", err)
	}

	claim, ok := item.Payload.(*controllerapi.OutputClaimData)
	if ok && claim.ReleasesInput && !strings.HasPrefix(claim.SourceKey, "budget:") {
		_ = q.manager.writeOutput(ctx, Event{
			SessionID: claim.SessionID, Type: "state_changed", Status: string(controllerapi.StateIdle),
		})
	}

	return nil
}

func (q *outputQueue) Retry(ctx context.Context, item *managerdelivery.Item, failure string, next time.Time) error {
	if err := q.RetryOutput(ctx, controllerapi.OutputRetryData{
		ID: item.ID, AttemptID: item.AttemptID, Error: failure, NextAt: next,
	}); err != nil {
		return fmt.Errorf("retry cli output: %w", err)
	}

	return nil
}

func (q *outputQueue) Block(ctx context.Context, item *managerdelivery.Item, failure string) error {
	if err := q.BlockOutput(ctx, controllerapi.OutputBlockData{
		ID: item.ID, AttemptID: item.AttemptID, Error: failure,
	}); err != nil {
		return fmt.Errorf("block cli output: %w", err)
	}

	return nil
}

type outputTransport struct{ manager *Manager }

func (t *outputTransport) Deliver(ctx context.Context, item *managerdelivery.Item) managerdelivery.Result {
	claim, ok := item.Payload.(*controllerapi.OutputClaimData)
	if !ok {
		return managerdelivery.Result{Error: "invalid cli output payload"}
	}

	event := Event{SessionID: claim.SessionID, Generation: item.ID, Type: claim.Type, Message: claim.Content}
	switch claim.Type {
	case controllerapi.OutputMessageReplaceable, controllerapi.OutputMessagePersistent:
		event.Type = "message"
		if _, waiting := claim.Attributes["waiting"]; waiting {
			event.Type = "waiting"
		}
	case controllerapi.OutputSessionOpened:
		t.manager.adoptLifecycle(0, claim.SessionID, item.ID)
	case controllerapi.OutputSessionReplaced:
		oldID, valid := positiveOutputID(claim.Attributes["old_session_id"])
		if !valid {
			return managerdelivery.Result{Error: "invalid cli session replacement"}
		}

		event.OldSessionID = oldID
		t.manager.adoptLifecycle(oldID, claim.SessionID, item.ID)
	case controllerapi.OutputSessionClosed:
		if t.manager.currentSession() == claim.SessionID {
			t.manager.adoptLifecycle(claim.SessionID, 0, item.ID)
		}
	default:
		return managerdelivery.Result{Error: "invalid cli output type"}
	}

	if err := t.manager.writeOutput(ctx, event); err != nil {
		return managerdelivery.Result{Retryable: true, Error: deliveryError(err)}
	}

	return managerdelivery.Result{MessageIDs: []string{}}
}

func deliveryError(err error) string {
	message := logger.Redact(strings.ReplaceAll(err.Error(), "\n", " "))
	if len(message) <= 512 {
		return message
	}

	message = message[:512]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}

	return message
}

func positiveOutputID(value any) (int64, bool) {
	switch number := value.(type) {
	case int64:
		return number, number > 0
	case int:
		return int64(number), number > 0
	case float64:
		return int64(number), number > 0 && number == float64(int64(number))
	default:
		return 0, false
	}
}
