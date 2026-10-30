package managercontrol

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/managerdiscovery"
	"github.com/pilat/coagent/internal/sessionbus"
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
	store Store,
	discovery *managerdiscovery.Service,
	bus *sessionbus.Bus,
	cfg *config.Config,
	cache *loader.MarketplaceCache,
) controllerapi.ManagerControllerFactory {
	return &factory{app: &service{
		backend: backend, store: store, discovery: discovery,
		bus: bus, cfg: cfg, cache: cache,
	}}
}

func (f *factory) ForManager(managerID string) controllerapi.Controller {
	return &controller{app: f.app, managerID: managerID}
}

func (c *controller) CreateSession(ctx context.Context, data controllerapi.SessionCreateData) (int64, error) {
	return c.app.createSession(ctx, c.managerID, data)
}

func (c *controller) SendSessionMessage(ctx context.Context, data controllerapi.SessionMessageData) error {
	return c.app.sendSessionMessage(ctx, c.managerID, data)
}

func (c *controller) ListSessions(ctx context.Context) ([]controllerapi.SessionInfo, error) {
	return c.app.listSessions(ctx, c.managerID)
}

func (c *controller) SetSessionModel(ctx context.Context, data controllerapi.SessionSetModelData) error {
	return c.app.setSessionModel(ctx, c.managerID, data)
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
