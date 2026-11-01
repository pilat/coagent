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

func (p *chatPort) Focus(ctx context.Context) (int64, error) {
	focus, err := p.controller.ChatFocus(ctx, p.chatID)
	if err != nil {
		return 0, fmt.Errorf("read telegram chat focus: %w", err)
	}

	return focus, nil
}

func (p *chatPort) ResetFocus(ctx context.Context, from int64) (bool, error) {
	reset, err := p.controller.ResetChatFocus(ctx, p.chatID, from)
	if err != nil {
		return false, fmt.Errorf("reset telegram chat focus: %w", err)
	}

	return reset, nil
}

func (p *chatPort) Outputs(ctx context.Context, sessionID int64, limit int) ([]managerdelivery.ChatRow, error) {
	rows, err := p.controller.ChatOutputs(ctx, p.chatID, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("read telegram chat outputs: %w", err)
	}

	outputs := make([]managerdelivery.ChatRow, 0, len(rows))
	for _, row := range rows {
		outputs = append(outputs, chatRow(row))
	}

	return outputs, nil
}

func (p *chatPort) LastSeen(ctx context.Context, sessionID int64) (managerdelivery.ChatRow, bool, error) {
	row, found, err := p.controller.LastSeenChatOutput(ctx, p.chatID, sessionID)
	if err != nil {
		return managerdelivery.ChatRow{}, false, fmt.Errorf("read telegram last seen chat output: %w", err)
	}

	return chatRow(row), found, nil
}

func (p *chatPort) MarkSeen(ctx context.Context, sessionID, outboxID int64) error {
	if err := p.controller.MarkChatSeen(ctx, p.chatID, sessionID, outboxID); err != nil {
		return fmt.Errorf("mark telegram chat seen: %w", err)
	}

	return nil
}

func (p *chatPort) Sessions(ctx context.Context) ([]managerdelivery.ChatSession, error) {
	rows, err := p.controller.ChatSessions(ctx, p.chatID)
	if err != nil {
		return nil, fmt.Errorf("read telegram chat sessions: %w", err)
	}

	sessions := make([]managerdelivery.ChatSession, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, managerdelivery.ChatSession{
			ID: row.ID, Name: row.Name, Unread: row.Unread, LatestUnread: row.LatestUnread,
			LatestUnreadID: row.LatestUnreadID,
			Scheduled:      row.Scheduled,
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

func chatRow(row controllerapi.ChatOutput) managerdelivery.ChatRow {
	return managerdelivery.ChatRow{
		ID: row.ID, Name: row.Name, Type: row.Type, Content: row.Content,
		Origin: row.Origin, OriginName: row.OriginName,
	}
}
