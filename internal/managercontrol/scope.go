package managercontrol

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

type projectScope []string

func (s projectScope) coversWorkDir(dir string) bool {
	return len(s) == 0 || slices.Contains(s, filepath.Clean(dir))
}

func (s projectScope) coversRoot(dir string, attrs map[string]any) bool {
	if s.coversWorkDir(dir) {
		return true
	}

	repo, _ := attrs["repo_root"].(string)

	return attrs[controllerapi.SessionAttributeWorktreeOrigin] == controllerapi.WorktreeOriginController &&
		s.coversWorkDir(repo)
}

func (c *appController) root(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	if err := requireManagerIdentity(c.operator.managerID); err != nil {
		return nil, err
	}

	if id <= 0 {
		return nil, fmt.Errorf("#%d is not a live session", id)
	}

	record, err := c.operator.app.store.GetSession(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("session %d not found", id)
	}

	if err != nil {
		return nil, fmt.Errorf("load session %d: %w", id, err)
	}

	dir, err := c.operator.app.store.GetProjectWorkDir(ctx, record.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("read session %d project: %w", id, err)
	}

	if !c.scope.coversRoot(dir, record.Attributes) {
		return nil, fmt.Errorf("session %d not found", id)
	}

	if record.ParentID != 0 {
		return nil, fmt.Errorf("session %d is not a root session", id)
	}

	return record, nil
}

func (c *appController) newest(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	chain, err := c.operator.app.store.ReplacementChain(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read root replacement chain: %w", err)
	}

	return c.root(ctx, chain[len(chain)-1])
}
