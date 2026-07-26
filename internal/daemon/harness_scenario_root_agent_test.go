package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
)

// A root session is the primary build agent. When the store lets the schema
// default decide the agent type, the root silently runs as the "general"
// subagent: it is told it is a subagent and loses the todo tools.
func TestHarnessScenario_RootSessionRunsAsTheBuildAgent(t *testing.T) {
	prompts := newPromptRecorder()

	respond := func(system string, _ []llmwire.Message) *llmwire.Response {
		prompts.record("root", system)

		return &llmwire.Response{Text: "done"}
	}

	h := newGatingHarness(t, nil, respond)
	defer h.shutdown()

	rootID, err := h.mgr.Send(h.ctx, h.projectID, "do the thing", "fake-model", nil)
	require.NoError(t, err)

	h.mgr.waitIdle(rootID)

	system := prompts.first(t, "root")
	assert.True(t, strings.HasPrefix(system, registry.BuildAgentPrompt),
		"root must open with the primary build prompt, got: %s", firstLine(system))
	assert.NotContains(t, system, "You are a subagent", "root is nobody's subagent")

	offered := h.schemas.offered(rootID)
	assert.Contains(t, offered, "todoread", "todo tools belong to the primary agent")
	assert.Contains(t, offered, "todowrite")

	rec, err := h.sessStore.GetSession(h.ctx, rootID)
	require.NoError(t, err)
	assert.Equal(t, string(registry.AgentTypeBuild), rec.AgentType,
		"the root row names the agent it runs")
}

func firstLine(s string) string {
	head, _, _ := strings.Cut(s, "\n")

	return head
}
