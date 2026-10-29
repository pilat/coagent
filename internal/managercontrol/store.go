package managercontrol

import (
	"context"

	"github.com/pilat/coagent/internal/sessionstore"
)

// Store is the persistence view required by managercontrol.
type Store interface {
	GetSession(context.Context, int64) (*sessionstore.SessionRecord, error)
	ListSessions(context.Context) ([]*sessionstore.SessionRecord, error)
	GetOrCreateProject(context.Context, string) (int64, error)
	GetOrCreateNamedProject(context.Context, string, string) (int64, error)
	GetProjectWorkDir(context.Context, int64) (string, error)
	GetProjectName(context.Context, int64) (string, error)
	ChatFocus(context.Context, string, string) (int64, error)
	SetChatFocus(context.Context, string, string, int64) error
	ResetChatFocus(context.Context, string, string, int64) (bool, error)
	ChatOutputs(context.Context, string, string, int64, int) ([]sessionstore.ChatOutput, error)
	MarkChatSeen(context.Context, string, string, int64, int64) error
	ChatSessions(context.Context, string, string) ([]sessionstore.ChatSession, error)
}
