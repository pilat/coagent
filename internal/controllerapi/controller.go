package controllerapi

import (
	"context"
)

// ManagerControllerFactory binds the private controller to one manager ID.
// The local API server receives each capability; ownership is not caller input.
//
//nolint:iface // factory capability consumed by composition roots in other packages
type ManagerControllerFactory interface {
	ForManager(managerID string) Controller
}

// ChatController groups the session and discovery calls served by the local API.
type ChatController interface {
	CreateSession(ctx context.Context, data SessionCreateData) (int64, error)
	SendSessionMessage(ctx context.Context, data SessionMessageData) error
	ListSessions(ctx context.Context) ([]SessionInfo, error)
	SetSessionModel(ctx context.Context, data SessionSetModelData) error
	ListModels(ctx context.Context) (*ConfigModelsResultData, error)
	CreateProject(ctx context.Context, data ProjectCreateData) (*ProjectCreateResultData, error)
	Subscribe() <-chan SessionNotification
	Unsubscribe(ch <-chan SessionNotification)
}

// Controller is the manager-bound capability the local API server serves;
// built-in managers reach it only through coagent.v1.
type Controller interface {
	ChatController
	SpawnFavorites(ctx context.Context) ([]string, error)
	ListSkills(ctx context.Context, data ConfigSkillsData) (*ConfigSkillsResultData, error)
	ListRecentProjects(ctx context.Context) (*ProjectListResultData, error)
}
