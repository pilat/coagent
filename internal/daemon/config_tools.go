package daemon

import (
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/configtools"
	"github.com/pilat/coagent/internal/tool"
)

// ConfigEditTool is unavailable without a config applier.
func (s *externalCalls) ConfigEditTool(sessionID int64) tool.Tool {
	if s.applier == nil {
		return nil
	}

	return configtools.NewConfigEdit(
		s.applier.Ops(),
		func(callID, toolName string, staged *configops.Staged) bool {
			return s.StageApply(sessionID, callID, toolName, staged)
		},
	)
}
