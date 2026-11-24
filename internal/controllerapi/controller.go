package controllerapi

import (
	"context"
)

// ManagerControllerFactory binds the private controller to one manager or
// API client id.
//
//nolint:iface // factory capability consumed by composition roots in other packages
type ManagerControllerFactory interface {
	ForManager(managerID string) Controller
	// ForApp binds an app's delegated authority; projects limits it, empty means every project.
	ForApp(appID string, projects []string) Controller
}

// ChatController groups the session and discovery calls served by the local API.
type ChatController interface {
	CreateSession(ctx context.Context, data SessionCreateData) (int64, error)
	SendSessionMessage(ctx context.Context, data SessionMessageData) error
	StopSession(ctx context.Context, data SessionStopData) (int64, error)
	KillSession(ctx context.Context, data SessionKillData) (int64, error)
	TurnOver(ctx context.Context, sessionID int64) (int64, bool, error)
	ListSessions(ctx context.Context) ([]SessionInfo, error)
	SetSessionModel(ctx context.Context, data SessionSetModelData) error
	ListModels(ctx context.Context) (*ConfigModelsResultData, error)
	CreateProject(ctx context.Context, data ProjectCreateData) (*ProjectCreateResultData, error)
	Subscribe() <-chan SessionNotification
	Unsubscribe(ch <-chan SessionNotification)
}

// Controller is the author-bound capability the local API server serves;
// built-in managers reach it only through coagent.v1.
type Controller interface {
	ChatController
	SpawnFavorites(ctx context.Context) ([]string, error)
	ListSkills(ctx context.Context, data ConfigSkillsData) (*ConfigSkillsResultData, error)
	ListRecentProjects(ctx context.Context) (*ProjectListResultData, error)
}
