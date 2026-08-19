package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (c *controller) requireManagerIdentity() error {
	if c.managerID == "" {
		return errors.New("controller has no manager identity")
	}

	return nil
}

func (c *controller) requireOwnedSession(ctx context.Context, sessionID int64) error {
	if err := c.requireManagerIdentity(); err != nil {
		return err
	}

	record, err := c.svc.GetSession(ctx, sessionID)
	if err != nil || record == nil {
		return fmt.Errorf("session %d not found", sessionID)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if owner != c.managerID {
		return fmt.Errorf("session %d belongs to another manager", sessionID)
	}

	return nil
}

func (c *controller) canListSession(ctx context.Context, record *sessionstore.SessionRecord) bool {
	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if owner == c.managerID && owner != "" {
		return true
	}

	return owner == "" && c.isLegacyCLISession(ctx, record)
}

func (c *controller) authorizeAttributeUpdate(
	ctx context.Context,
	data *controllerapi.SessionSetAttributesData,
) error {
	if err := c.requireManagerIdentity(); err != nil {
		return err
	}

	record, err := c.svc.GetSession(ctx, data.SessionID)
	if err != nil || record == nil {
		return fmt.Errorf("session %d not found", data.SessionID)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if owner != "" && owner != c.managerID {
		return fmt.Errorf("session %d belongs to another manager", data.SessionID)
	}

	if owner == "" && !c.isLegacyCLISession(ctx, record) {
		return fmt.Errorf("session %d has no claimable manager owner", data.SessionID)
	}

	data.Attributes = maps.Clone(data.Attributes)
	if data.Attributes == nil {
		data.Attributes = make(map[string]any)
	}

	data.Attributes[controllerapi.SessionAttributeManagerID] = c.managerID

	return nil
}

func (c *controller) isLegacyCLISession(ctx context.Context, record *sessionstore.SessionRecord) bool {
	if c.managerID != controllerapi.BuiltinCLIManagerID || record.ParentID != 0 {
		return false
	}

	channel, _ := record.Attributes["channel"].(string)
	if channel != controllerapi.BuiltinCLIManagerID {
		return false
	}

	name, err := c.svc.GetProjectName(ctx, record.ProjectID)
	if err != nil || name != controllerapi.CoagentSystemProjectName {
		return false
	}

	workDir, err := c.svc.GetProjectWorkDir(ctx, record.ProjectID)
	if err != nil {
		return false
	}

	var unified *config.UnifiedConfig
	if c.cfg != nil {
		unified = c.cfg.UnifiedConfig
	}

	expected := filepath.Join(resolveProjectsRoot(unified), controllerapi.CoagentSystemProjectDir)

	return sameProjectPath(workDir, expected)
}
