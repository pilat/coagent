package managerdiscovery

import (
	"context"
	"time"

	"github.com/pilat/coagent/internal/sessionstore"
)

type Backend interface {
	GetSession(context.Context, int64) (*sessionstore.SessionRecord, error)
	GetOrCreateProject(context.Context, string) (int64, error)
	GetOrCreateHiddenProject(context.Context, string) (int64, error)
	GetProjectWorkDir(context.Context, int64) (string, error)
	ListProjects(context.Context) ([]sessionstore.ProjectRow, error)
	LatestActivityByProject(context.Context, []int64) (map[int64]time.Time, error)
}
