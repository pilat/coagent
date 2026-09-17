package daemon

import (
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/configtools"
	"github.com/pilat/coagent/internal/tool"
)

// newConfigEditTool builds the user-authorized full-document editing tool. It
// lives on ordinary root sessions: the durable /config activation, not the
// reserved configuration project, is its authority.
func newConfigEditTool(s *svc, sessionID int64) tool.Tool {
	return configtools.NewConfigEdit(
		s.applier.Ops(),
		func(callID, toolName string, staged *configops.Staged) bool {
			return s.stageApply(sessionID, callID, toolName, staged)
		},
	)
}
