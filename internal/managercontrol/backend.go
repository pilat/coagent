package managercontrol

import (
	"context"

	"github.com/pilat/coagent/internal/sessionevent"
)

type Backend interface {
	CreateSession(context.Context, int64, string, string, string, map[string]any) (int64, error)
	SendToSessionResolved(context.Context, int64, string, string) (int64, error)
	SpawnForApp(context.Context, string, string, string, string) (int64, error)
	SendForApp(context.Context, string, int64, string, string) (int64, error)
	StopForApp(context.Context, string, int64) (int64, error)
	KillForApp(context.Context, string, int64) (int64, error)
	TurnOver(context.Context, int64) (int64, bool, error)
	SetModel(context.Context, int64, string, string) error
	HasActiveLoop(int64) bool
	NotifySession(int64, sessionevent.Notification)
}
