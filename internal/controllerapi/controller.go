package controllerapi

import (
	"context"
	"errors"
	"time"
)

// ErrNoOutput is the queue's normal empty-head result. It is deliberately
// transport-neutral so managers do not couple their worker to SQLite details.
var ErrNoOutput = errors.New("manager has no deliverable output")

type OutputRetryPendingError struct{ NextAt time.Time }

func (e *OutputRetryPendingError) Error() string { return "manager output retry is not due" }

// ManagerControllerFactory binds the private controller to one manager ID.
// Every manager receives a separate capability; ownership is not caller input.
//
//nolint:iface // factory capability consumed by composition roots in other packages
type ManagerControllerFactory interface {
	ForManager(managerID string) Controller
}

// OutputStatusFactory exposes redacted delivery health to the composition root.
//
//nolint:iface // composition root consumes this optional capability structurally.
type OutputStatusFactory interface {
	OutputQueueStatus(ctx context.Context, managerID string) (OutputQueueStatusData, error)
}

// OutputOwnerStatusFactory lists only manager IDs with unresolved durable
// output; status uses it to surface backlogs after manager removal.
//
//nolint:iface // composition root consumes this optional capability structurally.
type OutputOwnerStatusFactory interface {
	UnresolvedOutputOwners(ctx context.Context) ([]string, error)
}

// ChatController is the capability surface needed by an interactive chat
// transport. Keeping it separate means adding an administrative or discovery
// operation cannot break the CLI harness.
type ChatController interface {
	CreateSession(ctx context.Context, data SessionCreateData) (int64, error)
	SendSessionMessage(ctx context.Context, data SessionMessageData) error
	ListSessions(ctx context.Context) ([]SessionInfo, error)
	SetSessionModel(ctx context.Context, data SessionSetModelData) error
	SetSessionAttributes(ctx context.Context, data SessionSetAttributesData) error
	ListModels(ctx context.Context) (*ConfigModelsResultData, error)
	CreateProject(ctx context.Context, data ProjectCreateData) (*ProjectCreateResultData, error)
	Subscribe() <-chan SessionNotification
	Unsubscribe(ch <-chan SessionNotification)
}

// SessionMessageRouter returns the root session that durably accepted a
// message, which may be a replacement of the ID supplied by a terminal.
//
//nolint:iface // optional extension keeps ordinary ChatController fakes narrow.
type SessionMessageRouter interface {
	SendSessionMessageResolved(ctx context.Context, data SessionMessageData) (int64, error)
}

// Controller is the complete in-process API the daemon exposes to rich
// built-in managers such as Telegram. Narrower consumers should depend on a named
// capability interface such as ChatController instead of this aggregate.
type Controller interface {
	ChatController
	ListDir(ctx context.Context, data FsListDirData) (*FsListDirResultData, error)
	ListSkills(ctx context.Context, data ConfigSkillsData) (*ConfigSkillsResultData, error)
	ListRecentProjects(ctx context.Context) (*ProjectListResultData, error)
}

// OutputQueueController is the manager-bound durable delivery capability.
// Manager identity belongs to the capability returned by ForManager, never a
// request field.
//
//nolint:iface // optional capability, asserted by output-aware built-in managers.
type OutputQueueController interface {
	BindOutputDelivery(ctx context.Context, data OutputBindingData) error
	ClaimOutput(ctx context.Context) (*OutputClaimData, error)
	AckOutput(ctx context.Context, data OutputAckData) error
	RetryOutput(ctx context.Context, data OutputRetryData) error
	BlockOutput(ctx context.Context, data OutputBlockData) error
	WakeOutput(ctx context.Context) error
	RepairSessionSurface(ctx context.Context, sessionID int64, binding string) error
}
