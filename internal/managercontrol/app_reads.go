package managercontrol

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
)

func (c *appController) ChatOutputs(
	ctx context.Context,
	chat string,
	id, after int64,
	limit int,
) ([]controllerapi.ChatOutput, error) {
	if _, err := c.root(ctx, id); err != nil {
		return nil, err
	}

	return c.operator.ChatOutputs(ctx, chat, id, after, limit)
}

func (c *appController) ChatEntry(
	ctx context.Context,
	chat string,
	id int64,
	limit int,
) ([]controllerapi.ChatOutput, int64, error) {
	if _, err := c.root(ctx, id); err != nil {
		return nil, 0, err
	}

	return c.operator.ChatEntry(ctx, chat, id, limit)
}

func (c *appController) SessionKilled(ctx context.Context, id int64) (bool, error) {
	if _, err := c.root(ctx, id); err != nil {
		return false, err
	}

	return c.operator.SessionKilled(ctx, id)
}

func (c *appController) Successor(ctx context.Context, id int64) (int64, error) {
	if _, err := c.root(ctx, id); err != nil {
		return 0, err
	}

	return c.operator.Successor(ctx, id)
}

func (c *appController) Orchestrator(ctx context.Context, chat string, id int64) (bool, error) {
	if _, err := c.root(ctx, id); err != nil {
		return false, err
	}

	return c.operator.Orchestrator(ctx, chat, id)
}

func (c *appController) ListSkills(
	ctx context.Context,
	data controllerapi.ConfigSkillsData,
) (*controllerapi.ConfigSkillsResultData, error) {
	if data.SessionID > 0 {
		if _, err := c.root(ctx, data.SessionID); err != nil {
			return nil, err
		}
	}

	return c.operator.ListSkills(ctx, data)
}

func (c *appController) ListSessions(ctx context.Context) ([]controllerapi.SessionInfo, error) {
	rows, err := c.operator.ListSessions(ctx)
	if err != nil {
		return nil, err
	}

	var result []controllerapi.SessionInfo

	for _, row := range rows {
		if c.scope.coversRoot(row.WorkDir, row.Attributes) {
			result = append(result, row)
		}
	}

	return result, nil
}

func (c *appController) ChatSessions(ctx context.Context, chat string) ([]controllerapi.ChatSessionInfo, error) {
	rows, err := c.operator.ChatSessions(ctx, chat)
	if err != nil {
		return nil, err
	}

	var result []controllerapi.ChatSessionInfo

	for _, row := range rows {
		record, err := c.operator.app.store.GetSession(ctx, row.ID)
		if err != nil {
			return nil, fmt.Errorf("load listed session %d: %w", row.ID, err)
		}

		dir, err := c.operator.app.store.GetProjectWorkDir(ctx, record.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("read listed session %d project: %w", row.ID, err)
		}

		if c.scope.coversRoot(dir, record.Attributes) {
			result = append(result, row)
		}
	}

	return result, nil
}

func (c *appController) ListRecentProjects(ctx context.Context) (*controllerapi.ProjectListResultData, error) {
	result, err := c.operator.ListRecentProjects(ctx)
	if err != nil {
		return nil, err
	}

	var projects []controllerapi.RecentProjectInfo

	for _, row := range result.Projects {
		if c.scope.coversWorkDir(row.Path) {
			projects = append(projects, row)
		}
	}

	return &controllerapi.ProjectListResultData{Projects: projects}, nil
}

func (c *appController) SpawnFavorites(ctx context.Context) ([]string, error) {
	rows, err := c.operator.SpawnFavorites(ctx)
	if err != nil {
		return nil, err
	}

	var result []string

	for _, row := range rows {
		if c.scope.coversWorkDir(row) {
			result = append(result, row)
		}
	}

	return result, nil
}
