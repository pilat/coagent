package registry

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

func TestAssistantAgentType(t *testing.T) {
	t.Parallel()

	set := NewSet([]AgentTypeConfig{{Name: AgentTypeAssistant, Mode: ModeSubagent, Tools: []string{"*"}}})
	cfg, ok := set.Get(AgentTypeAssistant)
	require.True(t, ok)
	assert.Equal(t, sessionstore.AssistantAgentType, string(AgentTypeAssistant))
	assert.Equal(t, ModeHidden, cfg.Mode)
	assert.Equal(t, AssistantPrompt, cfg.Prompt)
	assert.True(t, cfg.OmitProjectContext)
	assert.True(t, cfg.DisableIntegratedSearch)
	assert.True(t, cfg.OmitFinalFooter)
	want := []string{
		"search", "around", "read_session", "sessions", "projects", "focus_list",
		"open", "send", "stop", "remember", "rewrite_memory", "forget_memory",
		"focus", "update_focus", "done_focus",
	}
	assert.Equal(t, want, cfg.Tools)
	all := append(slices.Clone(want), "bash", "read", "mcp__x__y", "task", "batch", "memory_save", "todowrite")
	assert.Equal(t, want, set.FilterTools(all, AgentTypeAssistant))
	for _, agent := range set.ListSubagents() {
		assert.NotEqual(t, AgentTypeAssistant, agent.Name)
	}
	browser, ok := set.Get(AgentTypeBrowser)
	require.True(t, ok)
	assert.True(t, browser.DisableIntegratedSearch)
	assert.False(t, browser.OmitFinalFooter)
}
