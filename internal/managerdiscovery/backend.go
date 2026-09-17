package managerdiscovery

import (
	"context"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

type Backend interface {
	GetSession(context.Context, int64) (*sessionstore.SessionRecord, error)
	GetOrCreateProject(context.Context, string) (int64, error)
	GetOrCreateHiddenProject(context.Context, string) (int64, error)
	GetProjectWorkDir(context.Context, int64) (string, error)
	ListHiddenProjectDirs(context.Context) ([]string, error)
	ListRecentProjects(context.Context, string) ([]controllerapi.RecentProjectInfo, error)
}
