package daemon

import (
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/configtools"
	"github.com/pilat/coagent/internal/tool"
)

func newConfigTools(s *svc, sessionID int64) []tool.Tool {
	return configtools.New(
		s.applier.Ops(),
		func(callID, toolName string, staged *configops.Staged) bool {
			return s.stageApply(sessionID, callID, toolName, staged)
		},
	)
}
