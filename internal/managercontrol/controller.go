package managercontrol

import (
	"context"
	"fmt"
	"path/filepath"

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
	cache *loader.MarketplaceCache,
) controllerapi.ManagerControllerFactory {
	return &factory{app: &service{
		backend: backend, store: store, discovery: discovery,
		bus: bus, cache: cache,
	}}
}

func (f *factory) ForManager(managerID string) controllerapi.Controller {
	return &controller{app: f.app, managerID: managerID}
}

func (f *factory) ForApp(appID string, projects []string) controllerapi.Controller {
	projectPaths := make(projectScope, 0, len(projects))
	for _, dir := range projects {
		projectPaths = append(projectPaths, filepath.Clean(dir))
	}

	return &appController{operator: &controller{app: f.app, managerID: appID}, scope: projectPaths}
}

func (c *controller) CreateSession(ctx context.Context, data controllerapi.SessionCreateData) (int64, error) {
	return c.app.createSession(ctx, c.managerID, data)
}

func (c *controller) SendSessionMessage(ctx context.Context, data controllerapi.SessionMessageData) error {
	_, err := c.app.sendSessionMessage(ctx, c.managerID, data)

	return err
}

func (c *controller) StopSession(ctx context.Context, data controllerapi.SessionStopData) (int64, error) {
	return c.app.sendSessionMessage(
		ctx,
		c.managerID,
		controllerapi.SessionMessageData{SessionID: data.SessionID, Message: "/stop"},
	)
}

func (c *controller) KillSession(ctx context.Context, data controllerapi.SessionKillData) (int64, error) {
	return c.app.sendSessionMessage(
		ctx,
		c.managerID,
		controllerapi.SessionMessageData{SessionID: data.SessionID, Message: "/kill"},
	)
}

func (c *controller) TurnOver(ctx context.Context, id int64) (int64, bool, error) {
	if _, err := c.chatSession(ctx, id); err != nil {
		return 0, false, err
	}

	resolved, over, err := c.app.backend.TurnOver(ctx, id)
	if err != nil {
		return 0, false, fmt.Errorf("read turn outcome: %w", err)
	}

	return resolved, over, nil
}

func (c *controller) ListSessions(ctx context.Context) ([]controllerapi.SessionInfo, error) {
	return c.app.listSessions(ctx, c.managerID)
}

func (c *controller) SetSessionModel(ctx context.Context, data controllerapi.SessionSetModelData) error {
	return c.app.setSessionModel(ctx, c.managerID, data)
}

func (c *controller) SpawnFavorites(ctx context.Context) ([]string, error) {
	result, err := c.app.discovery.SpawnFavorites(ctx)
	if err != nil {
		return nil, fmt.Errorf("spawn favorites: %w", err)
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
	if err := requireManagerIdentity(c.managerID); err != nil {
		return nil, err
	}

	result, err := c.app.discovery.ListSkills(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}

	return result, nil
}

func (c *controller) CreateProject(
	ctx context.Context,
	data controllerapi.ProjectCreateData,
) (*controllerapi.ProjectCreateResultData, error) {
	result, err := c.app.discovery.CreateProject(ctx, data)
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
	c.app.bus.UnsubscribeRoots(ch)
}
