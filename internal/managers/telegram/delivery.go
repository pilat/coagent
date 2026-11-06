package telegram

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managerdelivery"
)

var (
	_ managerdelivery.ChatPort      = (*chatPort)(nil)
	_ managerdelivery.ChatTransport = (*outputTransport)(nil)
)

type chatPort struct {
	controller controllerapi.ChatFocusController
	chatID     string
}

type outputTransport struct{ manager *Manager }

func (p *chatPort) Focus(ctx context.Context) (managerdelivery.FocusState, error) {
	session, cursor, entered, err := p.controller.ChatCursor(ctx, p.chatID)
	if err != nil {
		return managerdelivery.FocusState{}, fmt.Errorf("read telegram chat cursor: %w", err)
	}

	return managerdelivery.FocusState{Session: session, Cursor: cursor, Entered: entered}, nil
}

func (p *chatPort) ResetFocus(ctx context.Context, from int64) (bool, error) {
	reset, err := p.controller.ResetChatFocus(ctx, p.chatID, from)
	if err != nil {
		return false, fmt.Errorf("reset telegram chat focus: %w", err)
	}

	return reset, nil
}

func (p *chatPort) Outputs(ctx context.Context, sessionID, after int64, limit int) ([]managerdelivery.ChatRow, error) {
	rows, err := p.controller.ChatOutputs(ctx, p.chatID, sessionID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read telegram chat outputs: %w", err)
	}

	outputs := make([]managerdelivery.ChatRow, 0, len(rows))
	for _, row := range rows {
		outputs = append(outputs, chatRow(row))
	}

	return outputs, nil
}

func (p *chatPort) Sessions(ctx context.Context) ([]managerdelivery.ChatSession, error) {
	rows, err := p.controller.ChatSessions(ctx, p.chatID)
	if err != nil {
		return nil, fmt.Errorf("read telegram chat sessions: %w", err)
	}

	sessions := make([]managerdelivery.ChatSession, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, managerdelivery.ChatSession{
			ID: row.ID, Name: row.Name,
			Scheduled: row.Scheduled,
		})
	}

	return sessions, nil
}

func (p *chatPort) Killed(ctx context.Context, sessionID int64) (bool, error) {
	killed, err := p.controller.SessionKilled(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("read telegram session killed state: %w", err)
	}

	return killed, nil
}

func (t *outputTransport) Send(ctx context.Context, text string) managerdelivery.SendResult {
	if _, err := t.manager.sendMessage(ctx, text); err != nil {
		return classifyDeliveryError(err)
	}

	return managerdelivery.SendResult{}
}

func (p *chatPort) Entry(ctx context.Context, sessionID int64, limit int) ([]managerdelivery.ChatRow, int64, error) {
	rows, newest, err := p.controller.ChatEntry(ctx, p.chatID, sessionID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("read telegram chat entry: %w", err)
	}

	var outputs []managerdelivery.ChatRow
	for _, row := range rows {
		outputs = append(outputs, chatRow(row))
	}

	return outputs, newest, nil
}

func (p *chatPort) Enter(ctx context.Context, sessionID, newest int64) (bool, error) {
	entered, err := p.controller.EnterChat(ctx, p.chatID, sessionID, newest)
	if err != nil {
		return false, fmt.Errorf("enter telegram chat: %w", err)
	}

	return entered, nil
}

func (p *chatPort) Advance(ctx context.Context, sessionID, id int64) (bool, error) {
	advanced, err := p.controller.AdvanceChat(ctx, p.chatID, sessionID, id)
	if err != nil {
		return false, fmt.Errorf("advance telegram chat: %w", err)
	}

	return advanced, nil
}

func chatRow(row controllerapi.ChatOutput) managerdelivery.ChatRow {
	return managerdelivery.ChatRow{
		ID: row.ID, Name: row.Name, Type: row.Type, Content: row.Content,
	}
}
