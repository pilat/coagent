package managercontrol

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

var _ controllerapi.ChatFocusController = (*controller)(nil)

func (c *controller) BindChat(ctx context.Context, chatID string) error {
	if err := requireManagerIdentity(c.managerID); err != nil {
		return err
	}

	if err := c.app.store.BindChat(ctx, c.managerID, chatID); err != nil {
		return fmt.Errorf("bind manager chat: %w", err)
	}

	return nil
}

func (c *controller) ChatFocus(ctx context.Context, chatID string) (int64, error) {
	if err := requireManagerIdentity(c.managerID); err != nil {
		return 0, err
	}

	focus, err := c.app.store.ChatFocus(ctx, c.managerID, chatID)
	if err != nil {
		return 0, fmt.Errorf("read chat focus: %w", err)
	}

	return focus, nil
}

func (c *controller) SetChatFocus(ctx context.Context, chatID string, sessionID int64) error {
	if err := requireManagerIdentity(c.managerID); err != nil {
		return err
	}

	//nolint:wrapcheck // Rejections are the exact user-visible response.
	return c.app.store.SetChatFocus(ctx, c.managerID, chatID, sessionID)
}

func (c *controller) ResetChatFocus(ctx context.Context, chatID string, from int64) (bool, error) {
	record, err := c.chatSession(ctx, from)
	if err != nil {
		return false, err
	}

	reset, err := c.app.store.ResetChatFocus(ctx, c.managerID, chatID, record.ID)
	if err != nil {
		return false, fmt.Errorf("reset chat focus: %w", err)
	}

	return reset, nil
}

func (c *controller) SetSessionTitle(ctx context.Context, _ string, sessionID int64, raw string) error {
	if err := requireManagerIdentity(c.managerID); err != nil {
		return err
	}

	if sessionID == 0 {
		return fmt.Errorf("#%d is not a live session", sessionID)
	}

	title, ok := controllerapi.NormalizeSessionTitle(raw)
	if !ok {
		return errors.New("title must be one line of 1-80 characters")
	}

	//nolint:wrapcheck // Rejections are the exact user-visible response.
	return c.app.store.SetSessionTitle(ctx, c.managerID, sessionID, title)
}

func (c *controller) ChatOutputs(
	ctx context.Context,
	chatID string,
	sessionID int64,
	limit int,
) ([]controllerapi.ChatOutput, error) {
	record, err := c.chatSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	rows, err := c.app.store.ChatOutputs(ctx, c.managerID, chatID, record.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("read chat outputs: %w", err)
	}

	id := record.ID
	if record.AgentType == sessionstore.AssistantAgentType {
		id = 0
	}

	outputs := make([]controllerapi.ChatOutput, 0, len(rows))
	for _, row := range rows {
		outputs = append(outputs, controllerapi.ChatOutput{
			ID: row.ID, SessionID: id, Type: string(row.Type), Content: row.Content,
		})
	}

	return outputs, nil
}

func (c *controller) MarkChatSeen(ctx context.Context, chatID string, sessionID, outboxID int64) error {
	record, err := c.chatSession(ctx, sessionID)
	if err != nil {
		return err
	}

	if err := c.app.store.MarkChatSeen(ctx, c.managerID, chatID, record.ID, outboxID); err != nil {
		return fmt.Errorf("mark chat seen: %w", err)
	}

	return nil
}

func (c *controller) ChatSessions(ctx context.Context, chatID string) ([]controllerapi.ChatSessionInfo, error) {
	if err := requireManagerIdentity(c.managerID); err != nil {
		return nil, err
	}

	rows, err := c.app.store.ChatSessions(ctx, c.managerID, chatID)
	if err != nil {
		return nil, fmt.Errorf("read chat sessions: %w", err)
	}

	sessions := make([]controllerapi.ChatSessionInfo, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, controllerapi.ChatSessionInfo{
			ID: row.ID, SessionID: row.SessionID, Name: row.Name, Unread: row.Unread,
			LatestUnread: row.LatestUnread, LatestUnreadID: row.LatestUnreadID,
		})
	}

	return sessions, nil
}

func (c *controller) SessionKilled(ctx context.Context, sessionID int64) (bool, error) {
	record, err := c.chatSession(ctx, sessionID)
	if err != nil {
		return false, err
	}

	return record.KilledAt != nil, nil
}

func (c *controller) chatSession(ctx context.Context, sessionID int64) (*sessionstore.SessionRecord, error) {
	if err := requireManagerIdentity(c.managerID); err != nil {
		return nil, err
	}

	record, err := func() (*sessionstore.SessionRecord, error) {
		if sessionID == 0 {
			return c.app.backend.EnsureAssistantRoot(ctx, c.managerID)
		}

		return c.app.store.GetSession(ctx, sessionID)
	}()
	if err != nil {
		return nil, fmt.Errorf("load chat session %d: %w", sessionID, err)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if record.ParentID != 0 || owner != c.managerID {
		return nil, fmt.Errorf("session %d is not a root owned by this manager", sessionID)
	}

	return record, nil
}
