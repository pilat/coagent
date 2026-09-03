package managercontrol

import (
	"context"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

type sessionBackend interface {
	Send(context.Context, int64, string, string, map[string]any) (int64, error)
	SendToSessionResolved(context.Context, int64, string) (int64, error)
	GetSession(context.Context, int64) (*sessionstore.SessionRecord, error)
	List(context.Context) ([]*sessionstore.SessionRecord, error)
	SetModel(context.Context, int64, string, string) error
	SetAttributes(context.Context, int64, map[string]any) error
}

type runtimeBackend interface {
	HasActiveLoop(int64) bool
	PubSub() sessionbus.Source
	NotifySession(int64, sessionevent.Notification)
	CurrentProgress(context.Context, int64) (*controllerapi.ProgressData, error)
	RefreshProgress(context.Context, int64) error
	ReconcileOutputReadiness(context.Context, int64) error
}

type projectBackend interface {
	GetOrCreateProject(context.Context, string) (int64, error)
	GetOrCreateNamedProject(context.Context, string, string) (int64, error)
	GetOrCreateSystemProject(context.Context, string, string) (int64, error)
	GetProjectWorkDir(context.Context, int64) (string, error)
	GetProjectName(context.Context, int64) (string, error)
}

type Backend interface {
	sessionBackend
	projectBackend
	runtimeBackend
}
