package daemon

import (
	"context"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionlifecycle"
)

func newRunner(
	cancel context.CancelFunc,
	workDir string,
	projectID int64,
	kind admission.Kind,
	parentID int64,
	preserveStopped bool,
	inputs []queuedSessionInput,
) runner {
	return sessionlifecycle.NewRunner(cancel, workDir, projectID, kind, parentID, preserveStopped, inputs)
}

func transcriptOf(h *subagentHarness, sessionID int64) []llmwire.Message {
	messages, err := h.sessStore.LoadActiveMessages(h.ctx, sessionID)
	if err != nil {
		h.t.Fatalf("load transcript for session %d: %v", sessionID, err)
	}
	return toDTO(messages)
}

func newInputsRunner(projectID int64, inputs []queuedSessionInput) runner {
	return newRunner(func() {}, "", projectID, admission.Parent, 0, false, inputs)
}
