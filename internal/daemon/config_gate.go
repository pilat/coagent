package daemon

import (
	"context"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

// isCLISession reports whether a session belongs to a terminal, by the attribute
// the CLI manager stamps at creation.
func isCLISession(rec *sessionstore.SessionRecord) bool {
	channel, _ := rec.Attributes["channel"].(string)
	owner, _ := rec.Attributes[controllerapi.SessionAttributeManagerID].(string)

	return channel == controllerapi.BuiltinCLIManagerID &&
		(owner == "" || owner == controllerapi.BuiltinCLIManagerID)
}

func (s *svc) isConfigurationSession(ctx context.Context, rec *sessionstore.SessionRecord) bool {
	if rec.ParentID != 0 {
		return false
	}

	owner, _ := rec.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if owner != "" && owner != controllerapi.BuiltinCLIManagerID {
		return false
	}

	name, err := s.store.GetProjectName(ctx, rec.ProjectID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.config_gate").Warn(
			"project_identity_unavailable",
			zap.Int64("project_id", rec.ProjectID),
			zap.Error(err),
		)

		return false
	}

	if name != controllerapi.CoagentSystemProjectName {
		return false
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.config_gate").Warn(
			"project_path_unavailable",
			zap.Int64("project_id", rec.ProjectID),
			zap.Error(err),
		)

		return false
	}

	return sameProjectPath(workDir, s.systemProject)
}
