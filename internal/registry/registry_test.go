package registry

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrameSessionIsBrowserOnly(t *testing.T) {
	t.Parallel()

	set := NewSet(nil, BrowserSettings{})
	for name := range builtinAgentTypes {
		cfg, ok := set.Get(name)
		require.True(t, ok)
		assert.Equal(t, name == AgentTypeBrowser, cfg.FrameSession, "%s", name)
	}
}

// TestNewSet_IsolatesSameNamedSubagents guards the former global-registry bug:
// two sessions defining a same-named subagent with different configs must not
// clobber each other.
func TestNewSet_IsolatesSameNamedSubagents(t *testing.T) {
	name := AgentType("reviewer")

	setA := NewSet(
		[]AgentTypeConfig{{Name: name, Mode: ModeSubagent, Tools: []string{"read"}, Model: "model-a"}},
		BrowserSettings{},
	)
	setB := NewSet(
		[]AgentTypeConfig{{Name: name, Mode: ModeSubagent, Tools: []string{"bash"}, Model: "model-b"}},
		BrowserSettings{},
	)

	a, ok := setA.Get(name)
	require.True(t, ok)
	b, ok := setB.Get(name)
	require.True(t, ok)

	assert.Equal(t, "model-a", a.Model)
	assert.Equal(t, "model-b", b.Model)
	assert.Contains(t, a.Tools, "read")
	assert.NotContains(t, a.Tools, "bash")
	assert.Contains(t, b.Tools, "bash")
	assert.NotContains(t, b.Tools, "read")
}

func TestNewSet_NormalizesSubagents(t *testing.T) {
	name := AgentType("custom")

	set := NewSet([]AgentTypeConfig{{
		Name: name, Mode: ModeSubagent, Tools: []string{"*"}, Model: "model-a", Prompt: "custom prompt",
	}}, BrowserSettings{})

	cfg, ok := set.Get(name)
	require.True(t, ok)
	assert.Equal(t, "model-a", cfg.Model)
	assert.Equal(t, "custom prompt", cfg.Prompt)
	assert.Contains(t, cfg.Tools, "-todoread")
	assert.Contains(t, cfg.Tools, "-todowrite")
}

// An agent file without a `tools:` key is the ecosystem's "inherit everything"
// shorthand; it must not resolve to a spawnable but toolless agent.
func TestNewSet_SubagentWithoutToolsInheritsFullInventory(t *testing.T) {
	name := AgentType("docs-writer")

	set := NewSet([]AgentTypeConfig{{Name: name, Mode: ModeSubagent}}, BrowserSettings{})

	all := []string{"read", "write", "bash", "todoread", "todowrite"}
	assert.Equal(t, []string{"read", "write", "bash"}, set.FilterTools(all, name, func(string) bool { return false }))
}

func TestNewSet_SubagentWithExplicitlyEmptyToolsStaysToolless(t *testing.T) {
	name := AgentType("mute")

	set := NewSet([]AgentTypeConfig{{Name: name, Mode: ModeSubagent, Tools: []string{}}}, BrowserSettings{})

	assert.Empty(t, set.FilterTools([]string{"read", "write"}, name, func(string) bool { return false }))
}

func TestNewSet_ProjectSubagentShadowsBuiltIn(t *testing.T) {
	set := NewSet(
		[]AgentTypeConfig{{Name: AgentTypeGeneral, Mode: ModeSubagent, Description: "custom general"}},
		BrowserSettings{},
	)

	cfg, ok := set.Get(AgentTypeGeneral)
	require.True(t, ok)
	assert.Equal(t, "custom general", cfg.Description)
}

func TestNewSet_ProjectExploreOverrideKeepsFullContext(t *testing.T) {
	t.Parallel()

	builtins := NewSet(nil, BrowserSettings{})
	builtInExplore, ok := builtins.Get(AgentTypeExplore)
	require.True(t, ok)
	assert.True(t, builtInExplore.OmitProjectContext)

	overridden := NewSet([]AgentTypeConfig{{Name: AgentTypeExplore, Mode: ModeSubagent}}, BrowserSettings{})
	projectExplore, ok := overridden.Get(AgentTypeExplore)
	require.True(t, ok)
	assert.False(t, projectExplore.OmitProjectContext)
}

func TestNewSet_ClonesProjectToolsBeforeNormalization(t *testing.T) {
	t.Parallel()

	tools := make([]string, 1, 4)
	tools[0] = "read"
	set := NewSet([]AgentTypeConfig{{Name: "reviewer", Mode: ModeSubagent, Tools: tools}}, BrowserSettings{})

	tools[0] = "bash"
	tools = append(tools, "write")
	assert.Equal(t, "write", tools[1])

	cfg, ok := set.Get("reviewer")
	require.True(t, ok)
	assert.Equal(t, []string{"read", "-todoread", "-todowrite"}, cfg.Tools)
}

func TestSet_GetReturnsClonedTools(t *testing.T) {
	t.Parallel()

	set := NewSet(nil, BrowserSettings{})

	cfg, ok := set.Get(AgentTypeExplore)
	require.True(t, ok)
	cfg.Tools[0] = "bash"

	again, ok := set.Get(AgentTypeExplore)
	require.True(t, ok)
	assert.Equal(t, "read", again.Tools[0])
}

func TestSet_ListSubagentsReturnsClonedTools(t *testing.T) {
	t.Parallel()

	set := NewSet([]AgentTypeConfig{{Name: "reviewer", Mode: ModeSubagent, Tools: []string{"read"}}}, BrowserSettings{})

	configs := set.ListSubagents()
	for i := range configs {
		if configs[i].Name == "reviewer" {
			configs[i].Tools[0] = "bash"
		}
	}

	cfg, ok := set.Get("reviewer")
	require.True(t, ok)
	assert.Equal(t, "read", cfg.Tools[0])
}

func TestCloneConfigPreservesNilAndEmptyTools(t *testing.T) {
	t.Parallel()

	assert.Nil(t, cloneConfig(AgentTypeConfig{}).Tools)
	assert.NotNil(t, cloneConfig(AgentTypeConfig{Tools: []string{}}).Tools)
}

func TestSet_ListSubagentsDeterministic(t *testing.T) {
	set := NewSet([]AgentTypeConfig{
		{Name: "zeta", Mode: ModeSubagent},
		{Name: "alpha", Mode: ModeSubagent},
	}, BrowserSettings{})

	subagents := set.ListSubagents()
	names := make([]string, 0, len(subagents))
	for _, cfg := range subagents {
		names = append(names, string(cfg.Name))
	}

	// build (primary) and compaction (hidden) are excluded; result is name-sorted.
	assert.Equal(t, []string{"alpha", "browser", "explore", "general", "zeta"}, names)
}

func TestSet_FilterTools(t *testing.T) {
	set := NewSet(nil, BrowserSettings{})
	all := []string{"read", "write", "todoread", "todowrite", "bash"}

	general := set.FilterTools(all, AgentTypeGeneral, func(string) bool { return false })
	assert.Contains(t, general, "read")
	assert.Contains(t, general, "write")
	assert.NotContains(t, general, "todoread")
	assert.NotContains(t, general, "todowrite")

	explore := set.FilterTools(all, AgentTypeExplore, func(string) bool { return false })
	assert.Contains(t, explore, "read")
	assert.NotContains(t, explore, "write")
	assert.NotContains(t, explore, "bash")

	assert.Nil(t, set.FilterTools(all, AgentType("nonexistent"), func(string) bool { return false }))
}

func TestBrowserToolVisibility(t *testing.T) {
	isFrame := func(id string) bool { return strings.HasPrefix(id, "mcp__playwright__") }
	set := NewSet([]AgentTypeConfig{
		{Name: "custom", Mode: ModeSubagent, Tools: []string{"mcp__playwright__click", "read"}},
		{Name: AgentTypeBrowser, Mode: ModeSubagent, Tools: []string{"*"}, Description: "shadow"},
	}, BrowserSettings{})
	all := []string{"read", "mcp__playwright__click", "mcp__playwright__snapshot", "mcp__other__click"}
	for _, agent := range []AgentType{AgentTypeBuild, AgentTypeGeneral, "custom"} {
		assert.NotContains(t, set.FilterTools(all, agent, isFrame), "mcp__playwright__click")
		assert.NotContains(t, set.FilterTools(all, agent, isFrame), "mcp__playwright__snapshot")
	}
	assert.Equal(
		t,
		[]string{"mcp__playwright__click", "mcp__playwright__snapshot"},
		set.FilterTools(all, AgentTypeBrowser, isFrame),
	)
	cfg, ok := set.Get(AgentTypeBrowser)
	require.True(t, ok)
	assert.NotEqual(t, "shadow", cfg.Description)
	assert.True(t, cfg.OmitProjectContext)
}

func TestBrowserSettingsGrantOnlyWhitelistedBuiltins(t *testing.T) {
	isFrame := func(id string) bool { return strings.HasPrefix(id, "mcp__playwright__") }
	set := NewSet([]AgentTypeConfig{{Name: "custom", Mode: ModeSubagent, Tools: []string{"*"}}}, BrowserSettings{
		Model: "cheap", Effort: "low",
		Tools: []string{"read", "batch", "todowrite", "task", "mcp__other__x", "nope", "read"},
	})
	all := []string{"read", "write", "batch", "todowrite", "task", "mcp__other__x", "mcp__playwright__click"}

	cfg, ok := set.Get(AgentTypeBrowser)
	require.True(t, ok)
	assert.Equal(t, []string{"read"}, cfg.GrantedTools)
	assert.Equal(t, "cheap", cfg.Model)
	assert.True(t, cfg.PinnedModel)
	assert.Equal(t, "low", cfg.ReasoningLevel)
	assert.Equal(t,
		"Drive the configured browser surface. Also has built-in tools: read. "+browserDescriptionTail,
		cfg.Description)
	assert.Equal(t, []string{"read", "mcp__playwright__click"}, set.FilterTools(all, AgentTypeBrowser, isFrame))

	cfg.GrantedTools[0] = "mutated"
	again, _ := set.Get(AgentTypeBrowser)
	assert.Equal(t, []string{"read"}, again.GrantedTools)

	for _, agent := range []AgentType{AgentTypeGeneral, AgentTypeExplore, "custom"} {
		got, _ := set.Get(agent)
		assert.Empty(t, got.GrantedTools)
		assert.False(t, got.PinnedModel)
		assert.NotContains(t, set.FilterTools(all, agent, isFrame), "mcp__playwright__click")
	}
}

func TestBrowserSettingsKeepWhitelistOrder(t *testing.T) {
	cfg, _ := NewSet(nil, BrowserSettings{Tools: []string{"webfetch", "bash", "read"}}).Get(AgentTypeBrowser)
	assert.Equal(t, []string{"read", "bash", "webfetch"}, cfg.GrantedTools)
	assert.False(t, cfg.PinnedModel)
}

func TestEmptyBrowserSettingsKeepTodaysBrowser(t *testing.T) {
	isFrame := func(id string) bool { return strings.HasPrefix(id, "mcp__playwright__") }
	cfg, _ := NewSet(nil, BrowserSettings{}).Get(AgentTypeBrowser)
	assert.Equal(t,
		"Drive the configured browser surface with its tools only. Receives no project context; supply a "+
			"self-contained goal, constraints, starting URL or site, and the facts to return.",
		cfg.Description)
	assert.Nil(t, cfg.GrantedTools)
	assert.Empty(t, cfg.Model)
	assert.Empty(t, cfg.ReasoningLevel)
	assert.Equal(
		t,
		[]string{"mcp__playwright__click"},
		NewSet(
			nil,
			BrowserSettings{},
		).FilterTools([]string{"read", "mcp__playwright__click"}, AgentTypeBrowser, isFrame),
	)
}
