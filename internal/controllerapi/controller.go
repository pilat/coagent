package controllerapi

import "context"

// ChatController is the capability surface needed by an interactive chat
// transport. Keeping it separate means adding an administrative or discovery
// operation cannot break the CLI harness.
type ChatController interface {
	CreateSession(ctx context.Context, data SessionCreateData) (int64, error)
	SendSessionMessage(ctx context.Context, data SessionMessageData) error
	ListSessions(ctx context.Context) ([]SessionInfo, error)
	StopSession(ctx context.Context, data SessionStopData) error
	CreateProject(ctx context.Context, data ProjectCreateData) (*ProjectCreateResultData, error)
	SubscribeAll() <-chan SessionNotification
	UnsubscribeAll(ch <-chan SessionNotification)
}

// Controller is the complete in-process API the daemon exposes to rich
// built-in managers such as Telegram. Narrower consumers should depend on a named
// capability interface such as ChatController instead of this aggregate.
//
//nolint:iface // complete rich-controller aggregate; narrow consumers use capability interfaces
type Controller interface {
	ChatController
	KillSession(ctx context.Context, data SessionKillData) error
	ClearSession(ctx context.Context, data SessionClearData) (int64, error)
	SetSessionModel(ctx context.Context, data SessionSetModelData) error
	SetSessionAttributes(ctx context.Context, data SessionSetAttributesData) error
	ListDir(ctx context.Context, data FsListDirData) (*FsListDirResultData, error)
	ListModels(ctx context.Context) (*ConfigModelsResultData, error)
	ListSkills(ctx context.Context, data ConfigSkillsData) (*ConfigSkillsResultData, error)
	ListSchedules(ctx context.Context, data ScheduleListData) (*ScheduleListResultData, error)
	ListRecentProjects(ctx context.Context) (*ProjectListResultData, error)
}
