package registry

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
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
	want := []string{
		"search", "around", "read_session", "sessions", "projects", "focus_list",
		"open", "send", "stop", "remember", "rewrite_memory", "forget_memory",
		"focus", "update_focus", "done_focus",
		"switch", "models", "set_model",
		tool.IDSchedule, tool.IDMCPAdd, tool.IDMCPRemove, tool.IDMCPEnable, tool.IDMCPDisable, tool.IDMCPList,
		tool.IDConfigRead, tool.IDConfigEdit, "set_budget",
	}
	assert.Equal(t, want, cfg.Tools)
	all := append(slices.Clone(want), "bash", "read", "mcp__x__y", "task", "batch", "memory_save", "todowrite", "sleep")
	assert.Equal(t, want, set.FilterTools(all, AgentTypeAssistant))
	for _, agent := range set.ListSubagents() {
		assert.NotEqual(t, AgentTypeAssistant, agent.Name)
	}
	browser, ok := set.Get(AgentTypeBrowser)
	require.True(t, ok)
	assert.True(t, browser.DisableIntegratedSearch)
}

func TestAssistantPromptIncludesBoundedHostAdministration(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"## Host administration", "config_read", "config_edit", "current /config turn's grant",
		"complete application configuration", "anything missing from the document is deleted",
		"copied verbatim", "sandbox.rules", "sandbox.projects", "last matching rule",
		"mandatory read-only access to its process output", "deny: ~/.ssh", "agent socket",
		"no network boundary", "Never ask for a credential", "rotate",
		"/switch", "global MCP", "every turn kind", "no general file, shell or web access",
		"switch the user into a session", "they switch themselves", "they return with /assistant",
		"models to list configured model ids", "set_model(session, model, effort?)",
		"message beginning with /budget on #0", "only tool call in its response",
		"One /budget message permits one successful mutation", "Omit session for your own budget",
		"Never put slash commands into send/open", "instruction must not begin with /",
	} {
		assert.Contains(t, AssistantPrompt, text)
	}

	for _, text := range []string{"Telegram topic", "service topic", "kill picker", "coagent status", "only your final answer is published"} {
		assert.NotContains(t, AssistantPrompt, text)
	}
	assert.NotContains(t, AssistantPrompt, "set your own budget on /budget")
	assert.NotContains(t, AssistantPrompt, "Your schedule and budget tools affect your own assistant root")
}
