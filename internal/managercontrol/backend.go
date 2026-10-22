package managercontrol

import (
	"context"

	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

type Backend interface {
	EnsureAssistantRoot(context.Context, string) (*sessionstore.SessionRecord, error)
	Send(context.Context, int64, string, string, map[string]any) (int64, error)
	SendToSessionResolved(context.Context, int64, string) (int64, error)
	SetModel(context.Context, int64, string, string) error
	SetAttributes(context.Context, int64, map[string]any) error
	HasActiveLoop(int64) bool
	NotifySession(int64, sessionevent.Notification)
}
