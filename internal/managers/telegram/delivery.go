package telegram

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managerdelivery"
)

var (
	_ managerdelivery.ChatPort      = (*chatPort)(nil)
	_ managerdelivery.ChatTransport = (*outputTransport)(nil)
)

type chatPort struct {
	api    apiClients
	chatID string
}

type outputTransport struct{ manager *Manager }

func (p *chatPort) Focus(ctx context.Context) (managerdelivery.FocusState, error) {
	state, err := p.api.chats.GetChatCursor(ctx, &coagentv1.GetChatCursorRequest{ChatId: p.chatID})
	if err != nil {
		return managerdelivery.FocusState{}, fmt.Errorf("read telegram chat cursor: %w", err)
	}

	return managerdelivery.FocusState{
		Session: state.GetSessionId(),
		Cursor:  state.GetCursor(),
		Entered: state.GetEntered(),
	}, nil
}

func (p *chatPort) ResetFocus(ctx context.Context, from int64) (bool, error) {
	response, err := p.api.chats.ResetChatFocus(
		ctx,
		&coagentv1.ResetChatFocusRequest{ChatId: p.chatID, FromSessionId: from},
	)
	if err != nil {
		return false, fmt.Errorf("reset telegram chat focus: %w", err)
	}

	return response.GetCleared(), nil
}

func (p *chatPort) Outputs(ctx context.Context, sessionID, after int64, limit int) ([]managerdelivery.ChatRow, error) {
	response, err := p.api.outputs.ListOutput(
		ctx,
		&coagentv1.ListOutputRequest{SessionId: sessionID, AfterId: after, Limit: int32(limit)},
	)
	if err != nil {
		return nil, fmt.Errorf("read telegram chat outputs: %w", err)
	}

	outputs := make([]managerdelivery.ChatRow, 0, len(response.GetRows()))
	for _, row := range response.GetRows() {
		outputs = append(outputs, chatRow(row))
	}

	return outputs, nil
}

func (p *chatPort) Sessions(ctx context.Context) ([]managerdelivery.ChatSession, error) {
	response, err := p.api.sessions.ListSessions(ctx, &coagentv1.ListSessionsRequest{})
	if err != nil {
		return nil, fmt.Errorf("read telegram chat sessions: %w", err)
	}

	sessions := make([]managerdelivery.ChatSession, 0, len(response.GetSessions()))
	for _, row := range response.GetSessions() {
		sessions = append(sessions, managerdelivery.ChatSession{
			ID: row.GetId(), Name: row.GetName(),
			Scheduled: row.GetScheduled(),
		})
	}

	return sessions, nil
}

func (p *chatPort) Killed(ctx context.Context, sessionID int64) (bool, error) {
	response, err := p.api.sessions.GetSessionKilled(ctx, &coagentv1.GetSessionKilledRequest{SessionId: sessionID})
	if err != nil {
		return false, fmt.Errorf("read telegram session killed state: %w", err)
	}

	return response.GetKilled(), nil
}

func (t *outputTransport) Send(ctx context.Context, text string) managerdelivery.SendResult {
	if _, err := t.manager.sendMessage(ctx, text); err != nil {
		return classifyDeliveryError(err)
	}

	return managerdelivery.SendResult{}
}

func (p *chatPort) Entry(ctx context.Context, sessionID int64, limit int) ([]managerdelivery.ChatRow, int64, error) {
	response, err := p.api.outputs.ReadEntry(
		ctx,
		&coagentv1.ReadEntryRequest{SessionId: sessionID, Limit: int32(limit)},
	)
	if err != nil {
		return nil, 0, fmt.Errorf("read telegram chat entry: %w", err)
	}

	var outputs []managerdelivery.ChatRow
	for _, row := range response.GetRows() {
		outputs = append(outputs, chatRow(row))
	}

	return outputs, response.GetNewestId(), nil
}

func (p *chatPort) Enter(ctx context.Context, sessionID, newest int64) (bool, error) {
	response, err := p.api.chats.EnterChat(
		ctx,
		&coagentv1.EnterChatRequest{ChatId: p.chatID, SessionId: sessionID, NewestId: newest},
	)
	if err != nil {
		return false, fmt.Errorf("enter telegram chat: %w", err)
	}

	return response.GetEntered(), nil
}

func (p *chatPort) Advance(ctx context.Context, sessionID, id int64) (bool, error) {
	response, err := p.api.chats.AdvanceChat(
		ctx,
		&coagentv1.AdvanceChatRequest{ChatId: p.chatID, SessionId: sessionID, OutputId: id},
	)
	if err != nil {
		return false, fmt.Errorf("advance telegram chat: %w", err)
	}

	return response.GetAdvanced(), nil
}

func chatRow(row *coagentv1.OutputRow) managerdelivery.ChatRow {
	return managerdelivery.ChatRow{
		ID: row.GetId(), Name: row.GetSessionName(), Type: outputRowType(row.GetType()), Content: row.GetContent(),
	}
}

func outputRowType(kind coagentv1.OutputType) string {
	switch kind {
	case coagentv1.OutputType_OUTPUT_TYPE_MESSAGE:
		return controllerapi.OutputMessage
	case coagentv1.OutputType_OUTPUT_TYPE_SESSION_OPENED:
		return controllerapi.OutputSessionOpened
	case coagentv1.OutputType_OUTPUT_TYPE_SESSION_REPLACED:
		return controllerapi.OutputSessionReplaced
	case coagentv1.OutputType_OUTPUT_TYPE_SESSION_CLOSED:
		return controllerapi.OutputSessionClosed
	case coagentv1.OutputType_OUTPUT_TYPE_UNSPECIFIED:
		return ""
	}

	return ""
}
