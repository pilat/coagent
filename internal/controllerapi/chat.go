package controllerapi

import "context"

// ChatFocusController binds chat state to one manager without widening Controller.
//
//nolint:iface // optional capability asserted by built-in chat managers.
type ChatFocusController interface {
	BindChat(ctx context.Context, chatID string) error
	ChatFocus(ctx context.Context, chatID string) (int64, error)
	ChatCursor(ctx context.Context, chatID string) (int64, int64, bool, error)
	ChatEntry(ctx context.Context, chatID string, sessionID int64, limit int) ([]ChatOutput, int64, error)
	EnterChat(ctx context.Context, chatID string, sessionID, newest int64) (bool, error)
	AdvanceChat(ctx context.Context, chatID string, sessionID, id int64) (bool, error)
	SetChatFocus(ctx context.Context, chatID string, sessionID int64) error
	SetSessionTitle(ctx context.Context, chatID string, sessionID int64, title string) error
	ResetChatFocus(ctx context.Context, chatID string, from int64) (bool, error)
	ChatOutputs(ctx context.Context, chatID string, sessionID, after int64, limit int) ([]ChatOutput, error)
	ChatSessions(ctx context.Context, chatID string) ([]ChatSessionInfo, error)
	SessionKilled(ctx context.Context, sessionID int64) (bool, error)
}

type ChatOutput struct {
	ID        int64  `json:"id"`
	SessionID int64  `json:"session_id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Content   string `json:"content"`
}

// ChatSessionInfo describes a live manager-owned root.
type ChatSessionInfo struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Scheduled bool   `json:"scheduled"`
}
