package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managerdelivery"
)

type outputQueue struct {
	controllerapi.OutputQueueController
}

func newOutputQueue(controller controllerapi.OutputQueueController) *outputQueue {
	return &outputQueue{OutputQueueController: controller}
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
		return nil, fmt.Errorf("claim telegram output: %w", err)
	}

	return &managerdelivery.Item{
		ID: claim.ID, AttemptID: claim.AttemptID, Attempts: claim.AttemptSeq, Payload: claim,
	}, nil
}

func (q *outputQueue) Ack(ctx context.Context, item *managerdelivery.Item, result managerdelivery.Result) error {
	if err := q.AckOutput(ctx, controllerapi.OutputAckData{
		ID: item.ID, AttemptID: item.AttemptID, MessageIDs: result.MessageIDs, SessionPatch: result.SessionPatch,
	}); err != nil {
		return fmt.Errorf("ack telegram output: %w", err)
	}

	return nil
}

func (q *outputQueue) Retry(ctx context.Context, item *managerdelivery.Item, failure string, next time.Time) error {
	if err := q.RetryOutput(ctx, controllerapi.OutputRetryData{
		ID: item.ID, AttemptID: item.AttemptID, Error: failure, NextAt: next,
	}); err != nil {
		return fmt.Errorf("retry telegram output: %w", err)
	}

	return nil
}

func (q *outputQueue) Block(ctx context.Context, item *managerdelivery.Item, failure string) error {
	if err := q.BlockOutput(ctx, controllerapi.OutputBlockData{
		ID: item.ID, AttemptID: item.AttemptID, Error: failure,
	}); err != nil {
		return fmt.Errorf("block telegram output: %w", err)
	}

	return nil
}

type outputTransport struct{ manager *Manager }

func (t *outputTransport) Deliver(ctx context.Context, item *managerdelivery.Item) managerdelivery.Result {
	claim, ok := item.Payload.(*controllerapi.OutputClaimData)
	if !ok {
		return managerdelivery.Result{Error: "invalid durable output payload"}
	}

	switch claim.Type {
	case controllerapi.OutputSessionOpened:
		return t.openSession(ctx, claim)
	case controllerapi.OutputSessionReplaced:
		return t.replaceSession(ctx, claim)
	case controllerapi.OutputSessionClosed:
		return t.closeSession(ctx, claim)
	case controllerapi.OutputMessageReplaceable, controllerapi.OutputMessagePersistent:
	default:
		return managerdelivery.Result{Error: "unsupported durable output type"}
	}

	topicID, patch, err := t.ensureSessionTopic(ctx, claim)
	if err != nil {
		return t.deliveryFailure(err)
	}

	messageIDs, err := t.renderMessage(ctx, claim, topicID)
	if err != nil {
		return t.deliveryFailure(err)
	}

	return managerdelivery.Result{MessageIDs: messageIDs, SessionPatch: patch}
}

func (t *outputTransport) renderMessage(
	ctx context.Context,
	claim *controllerapi.OutputClaimData,
	topicID int64,
) ([]string, error) {
	chunks := splitMessageChunks(textToTelegramHTML(claim.Content), maxMessageChunk)
	previous := previousMessageIDs(claim)

	editPrevious := claim.PreviousMessageType == controllerapi.OutputMessageReplaceable && len(previous) > 0
	if !editPrevious {
		return t.sendChunks(ctx, chunks, topicID)
	}

	common := min(len(previous), len(chunks))
	for i := range common {
		messageID, err := strconv.ParseInt(previous[i], 10, 64)
		if err != nil {
			return t.sendChunks(ctx, chunks, topicID)
		}

		if err := t.manager.editMessageRawHTML(ctx, messageID, chunks[i], nil); err != nil {
			if isMessageNotModified(err) {
				continue
			}

			if isMessageMissing(err) {
				// The missing target proves nothing about the receipts behind
				// it; leaving them alive would leak them into the topic forever.
				if err := t.deleteReceipts(ctx, previous[i+1:]); err != nil {
					return nil, err
				}

				return t.sendChunks(ctx, chunks, topicID)
			}

			return nil, fmt.Errorf("edit output chunk: %w", err)
		}
	}

	ids := append([]string(nil), previous[:common]...)
	if len(chunks) > common {
		more, err := t.sendChunks(ctx, chunks[common:], topicID)
		if err != nil {
			return nil, err
		}

		ids = append(ids, more...)
	}

	if err := t.deleteReceipts(ctx, previous[common:]); err != nil {
		return nil, err
	}

	return ids, nil
}

func (t *outputTransport) deleteReceipts(ctx context.Context, ids []string) error {
	for _, id := range ids {
		messageID, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			continue
		}

		if err := t.manager.deleteMessage(ctx, messageID); err != nil && !isMessageMissing(err) {
			return fmt.Errorf("delete surplus output chunk: %w", err)
		}
	}

	return nil
}

func (t *outputTransport) sendChunks(ctx context.Context, chunks []string, topicID int64) ([]string, error) {
	ids := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		id, err := t.manager.sendMessageChunk(ctx, chunk, nil, topicID)
		if err != nil && shouldFallbackToPlain(err) {
			id, err = t.manager.sendPlainFallback(ctx, chunk, nil, topicID)
		}

		if err != nil {
			return nil, fmt.Errorf("send output chunk: %w", err)
		}

		ids = append(ids, strconv.FormatInt(id, 10))
	}

	return ids, nil
}

func previousMessageIDs(claim *controllerapi.OutputClaimData) []string {
	values, ok := claim.PreviousMessageAttributes["message_ids"].([]any)
	if !ok {
		return nil
	}

	ids := make([]string, 0, len(values))
	for _, value := range values {
		id, ok := value.(string)
		if !ok || id == "" {
			return nil
		}

		ids = append(ids, id)
	}

	return ids
}
