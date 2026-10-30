package controllerapi

import "context"

// ChatFocusController binds chat state to one manager without widening Controller.
//
//nolint:iface // optional capability asserted by built-in chat managers.
type ChatFocusController interface {
	BindChat(ctx context.Context, chatID string) error
	ChatFocus(ctx context.Context, chatID string) (int64, error)
	SetChatFocus(ctx context.Context, chatID string, sessionID int64) error
	SetSessionTitle(ctx context.Context, chatID string, sessionID int64, title string) error
	ResetChatFocus(ctx context.Context, chatID string, from int64) (bool, error)
	ChatOutputs(ctx context.Context, chatID string, sessionID int64, limit int) ([]ChatOutput, error)
	LastSeenChatOutput(ctx context.Context, chatID string, sessionID int64) (ChatOutput, bool, error)
	MarkChatSeen(ctx context.Context, chatID string, sessionID, outboxID int64) error
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

// ChatSessionInfo keeps the assistant's public #0 separate from its durable root ID.
type ChatSessionInfo struct {
	ID             int64  `json:"id"`
	SessionID      int64  `json:"session_id"`
	Name           string `json:"name"`
	Unread         int64  `json:"unread"`
	LatestUnread   string `json:"latest_unread"`
	LatestUnreadID int64  `json:"latest_unread_id"`
	Scheduled      bool   `json:"scheduled"`
}
