package controllerapi

import (
	"context"
)

// ManagerControllerFactory binds the private controller to one manager ID.
// Every manager receives a separate capability; ownership is not caller input.
//
//nolint:iface // factory capability consumed by composition roots in other packages
type ManagerControllerFactory interface {
	ForManager(managerID string) Controller
}

// ChatController is the capability surface needed by an interactive chat
// transport. Keeping it separate means adding an administrative or discovery
// operation cannot break existing manager harnesses.
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

// AssistantRootEnsureData identifies the manager's durable assistant root.
type AssistantRootEnsureData struct {
	SessionID int64  `json:"session_id"`
	WorkDir   string `json:"work_dir"`
}

// AssistantController exposes the manager's assistant identity.
type AssistantController interface {
	EnsureAssistantRoot(context.Context) (*AssistantRootEnsureData, error)
}

// Controller is the complete in-process API the daemon exposes to rich
// built-in managers such as Telegram. Narrower consumers should depend on a named
// capability interface such as ChatController instead of this aggregate.
type Controller interface {
	ChatController
	AssistantController
	ListDir(ctx context.Context, data FsListDirData) (*FsListDirResultData, error)
	ListSkills(ctx context.Context, data ConfigSkillsData) (*ConfigSkillsResultData, error)
	ListRecentProjects(ctx context.Context) (*ProjectListResultData, error)
}
