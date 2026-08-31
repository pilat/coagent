package managercontrol

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/managerdiscovery"
	"github.com/pilat/coagent/internal/sessionstore"
)

var (
	_ controllerapi.ManagerControllerFactory = (*factory)(nil)
	_ controllerapi.Controller               = (*controller)(nil)
)

type factory struct{ app *service }

type controller struct {
	app       *service
	managerID string
}

func New(
	backend Backend,
	discoveryBackend managerdiscovery.Backend,
	outputs sessionstore.ManagerOutputStore,
	cfg *config.Config,
	cache loader.MarketplaceCache,
) controllerapi.ManagerControllerFactory {
	return &factory{app: newService(backend, discoveryBackend, outputs, cfg, cache)}
}

func (f *factory) ForManager(managerID string) controllerapi.Controller {
	return &controller{app: f.app, managerID: managerID}
}

func (f *factory) OutputQueueStatus(
	ctx context.Context,
	managerID string,
) (controllerapi.OutputQueueStatusData, error) {
	return f.app.outputQueueStatus(ctx, managerID)
}

func (f *factory) UnresolvedOutputOwners(ctx context.Context) ([]string, error) {
	return f.app.unresolvedOutputOwners(ctx)
}

func (c *controller) BindOutputDelivery(ctx context.Context, data controllerapi.OutputBindingData) error {
	return c.app.bindOutputDelivery(ctx, c.managerID, data)
}

func (c *controller) ClaimOutput(ctx context.Context) (*controllerapi.OutputClaimData, error) {
	return c.app.claimOutput(ctx, c.managerID)
}

func (c *controller) AckOutput(ctx context.Context, data controllerapi.OutputAckData) error {
	return c.app.ackOutput(ctx, c.managerID, data)
}

func (c *controller) RetryOutput(ctx context.Context, data controllerapi.OutputRetryData) error {
	return c.app.retryOutput(ctx, c.managerID, data)
}

func (c *controller) BlockOutput(ctx context.Context, data controllerapi.OutputBlockData) error {
	return c.app.blockOutput(ctx, c.managerID, data)
}

func (c *controller) WakeOutput(ctx context.Context) error {
	return c.app.wakeOutput(ctx, c.managerID)
}

func (c *controller) RepairSessionSurface(ctx context.Context, sessionID int64, binding string) error {
	return c.app.repairSessionSurface(ctx, c.managerID, sessionID, binding)
}

func (c *controller) CurrentProgress(
	ctx context.Context,
	sessionID int64,
) (*controllerapi.ProgressData, error) {
	return c.app.currentProgress(ctx, c.managerID, sessionID)
}

func (c *controller) RefreshProgress(ctx context.Context, sessionID int64) error {
	return c.app.refreshProgress(ctx, c.managerID, sessionID)
}

func (c *controller) CreateSession(ctx context.Context, data controllerapi.SessionCreateData) (int64, error) {
	return c.app.createSession(ctx, c.managerID, data)
}

func (c *controller) SendSessionMessage(ctx context.Context, data controllerapi.SessionMessageData) error {
	_, err := c.app.sendSessionMessage(ctx, c.managerID, data)

	return err
}

func (c *controller) SendSessionMessageResolved(
	ctx context.Context,
	data controllerapi.SessionMessageData,
) (int64, error) {
	return c.app.sendSessionMessage(ctx, c.managerID, data)
}

func (c *controller) ListSessions(ctx context.Context) ([]controllerapi.SessionInfo, error) {
	return c.app.listSessions(ctx, c.managerID)
}

func (c *controller) SetSessionModel(ctx context.Context, data controllerapi.SessionSetModelData) error {
	return c.app.setSessionModel(ctx, c.managerID, data)
}

func (c *controller) SetSessionAttributes(
	ctx context.Context,
	data controllerapi.SessionSetAttributesData,
) error {
	return c.app.setSessionAttributes(ctx, c.managerID, data)
}

func (c *controller) ListDir(
	ctx context.Context,
	data controllerapi.FsListDirData,
) (*controllerapi.FsListDirResultData, error) {
	result, err := c.app.discovery.ListDir(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("list directory: %w", err)
	}

	return result, nil
}

func (c *controller) ListModels(ctx context.Context) (*controllerapi.ConfigModelsResultData, error) {
	result, err := c.app.discovery.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}

	return result, nil
}

func (c *controller) ListSkills(
	ctx context.Context,
	data controllerapi.ConfigSkillsData,
) (*controllerapi.ConfigSkillsResultData, error) {
	result, err := c.app.discovery.ListSkills(ctx, c.managerID, data)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}

	return result, nil
}

func (c *controller) CreateProject(
	ctx context.Context,
	data controllerapi.ProjectCreateData,
) (*controllerapi.ProjectCreateResultData, error) {
	result, err := c.app.discovery.CreateProject(ctx, c.managerID, data)
	if err != nil {
		return nil, fmt.Errorf("create project: %w", err)
	}

	return result, nil
}

func (c *controller) ListRecentProjects(ctx context.Context) (*controllerapi.ProjectListResultData, error) {
	result, err := c.app.discovery.ListRecentProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list recent projects: %w", err)
	}

	return result, nil
}

func (c *controller) Subscribe() <-chan controllerapi.SessionNotification {
	return c.app.subscribe(c.managerID)
}

func (c *controller) Unsubscribe(ch <-chan controllerapi.SessionNotification) {
	c.app.unsubscribe(ch)
}
