package managercontrol

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/pilat/coagent/internal/controllerapi"
)

var (
	_ controllerapi.Controller          = (*appController)(nil)
	_ controllerapi.ChatFocusController = (*appController)(nil)
	_ controllerapi.LineageController   = (*appController)(nil)
)

type appController struct {
	operator *controller
	scope    projectScope
}

func (c *appController) CreateSession(ctx context.Context, data controllerapi.SessionCreateData) (int64, error) {
	dir := filepath.Clean(data.WorkDir)
	if !c.scope.coversWorkDir(dir) {
		return 0, fmt.Errorf("no project at %q", dir)
	}

	if err := reservedAttributes(data.Attributes); err != nil {
		return 0, err
	}

	id, err := c.operator.app.backend.SpawnForApp(ctx, c.operator.managerID, dir, data.WorktreeName, data.Model)
	if err != nil {
		return 0, fmt.Errorf("spawn app session: %w", err)
	}

	return id, nil
}

func (c *appController) SendSessionMessage(ctx context.Context, data controllerapi.SessionMessageData) error {
	if _, err := c.root(ctx, data.SessionID); err != nil {
		return err
	}

	root, err := c.newest(ctx, data.SessionID)
	if err != nil {
		return err
	}

	if len(c.scope) != 0 && controllerapi.OrchestratorMode(root.Attributes) {
		return fmt.Errorf("orchestrator mode is on for session #%d; a project-scoped app cannot send to it", root.ID)
	}

	_, err = c.operator.app.backend.SendForApp(
		ctx,
		c.operator.managerID,
		data.SessionID,
		data.Message,
		authorKey(c.operator.managerID, data.IdempotencyKey),
	)
	if err != nil {
		return fmt.Errorf("send app message: %w", err)
	}

	return nil
}

func (c *appController) StopSession(ctx context.Context, data controllerapi.SessionStopData) (int64, error) {
	if _, err := c.root(ctx, data.SessionID); err != nil {
		return 0, err
	}

	resolved, err := c.operator.app.backend.StopForApp(ctx, c.operator.managerID, data.SessionID)
	if err != nil {
		return 0, fmt.Errorf("stop app session: %w", err)
	}

	return resolved, nil
}

func (c *appController) KillSession(ctx context.Context, data controllerapi.SessionKillData) (int64, error) {
	if _, err := c.root(ctx, data.SessionID); err != nil {
		return 0, err
	}

	resolved, err := c.operator.app.backend.KillForApp(ctx, c.operator.managerID, data.SessionID)
	if err != nil {
		return 0, fmt.Errorf("kill app session: %w", err)
	}

	return resolved, nil
}

func (c *appController) TurnOver(ctx context.Context, id int64) (int64, bool, error) {
	if _, err := c.root(ctx, id); err != nil {
		return 0, false, err
	}

	return c.operator.TurnOver(ctx, id)
}

func (c *appController) Subscribe() <-chan controllerapi.SessionNotification {
	return c.operator.Subscribe()
}

func (c *appController) Unsubscribe(ch <-chan controllerapi.SessionNotification) {
	c.operator.Unsubscribe(ch)
}

func (c *appController) ListModels(ctx context.Context) (*controllerapi.ConfigModelsResultData, error) {
	return c.operator.ListModels(ctx)
}

func (*appController) SetSessionModel(context.Context, controllerapi.SessionSetModelData) error {
	return errors.New("an app cannot change a session's model")
}

func (*appController) SetSessionTitle(context.Context, string, int64, string) error {
	return errors.New("an app cannot rename a session")
}

func (*appController) SetOrchestrator(context.Context, string, int64, bool) (bool, error) {
	return false, errors.New("an app cannot change orchestrator mode")
}

func (*appController) CreateProject(
	context.Context,
	controllerapi.ProjectCreateData,
) (*controllerapi.ProjectCreateResultData, error) {
	return nil, errors.New("an app cannot create projects")
}

func (*appController) BindChat(context.Context, string) error {
	return errors.New("an app keeps no chat; follow a root with TailOutput")
}

func (*appController) ChatFocus(context.Context, string) (int64, error) {
	return 0, errors.New("an app keeps no chat; follow a root with TailOutput")
}

func (*appController) ChatCursor(context.Context, string) (int64, int64, bool, error) {
	return 0, 0, false, errors.New("an app keeps no chat; follow a root with TailOutput")
}

func (*appController) EnterChat(context.Context, string, int64, int64) (bool, error) {
	return false, errors.New("an app keeps no chat; follow a root with TailOutput")
}

func (*appController) AdvanceChat(context.Context, string, int64, int64) (bool, error) {
	return false, errors.New("an app keeps no chat; follow a root with TailOutput")
}

func (*appController) SetChatFocus(context.Context, string, int64) error {
	return errors.New("an app keeps no chat; follow a root with TailOutput")
}

func (*appController) ResetChatFocus(context.Context, string, int64) (bool, error) {
	return false, errors.New("an app keeps no chat; follow a root with TailOutput")
}
