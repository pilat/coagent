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
		"open", "send", "stop", "kill", "remember", "rewrite_memory", "forget_memory",
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
		"only tool call in its response", "Omit session for your own budget",
		"change your own budget first", "/skill <name> [args]", "on an ordinary request",
		"/config and /budget sent there grant you nothing", "/config on #0, never inside a work session",
		"refuses send until you raise or clear its budget",
		"open(project, about, text, model?, effort?, worktree?)", "never open and then set_model",
		"sessions and the desk show model · effort and budget", "action=get gives the budget state",
		"or the user talked to directly", "⏰", "exactly one of cron or at",
		"Set report only when the user wants to be told the result", "schedule #N fired",
		"list shows yours and work sessions' schedules", "cancel removes one by id",
		"one short line without retelling", "freshly fetched remote default branch",
		"Never tell a session to create a worktree itself", "for a config refusal, suggest /gwt",
		"stop pauses work that can be resumed", "kill deletes a session for good",
		"only when the user asks to delete or remove it",
	} {
		assert.Contains(t, AssistantPrompt, text)
	}

	for _, text := range []string{"Telegram topic", "service topic", "kill picker", "coagent status", "only your final answer is published"} {
		assert.NotContains(t, AssistantPrompt, text)
	}
	assert.NotContains(t, AssistantPrompt, "set your own budget on /budget")
	assert.NotContains(t, AssistantPrompt, "Your schedule and budget tools affect your own assistant root")
	assert.NotContains(t, AssistantPrompt, "commands belong to the user")
	assert.NotContains(t, AssistantPrompt, "/etc/nginx.conf")
}
