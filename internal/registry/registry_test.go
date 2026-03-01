package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewSet_IsolatesSameNamedSubagents guards the former global-registry bug:
// two sessions defining a same-named subagent with different configs must not
// clobber each other.
func TestNewSet_IsolatesSameNamedSubagents(t *testing.T) {
	name := AgentType("reviewer")

	setA := NewSet([]AgentTypeConfig{{Name: name, Mode: ModeSubagent, Tools: []string{"read"}, Model: "model-a"}})
	setB := NewSet([]AgentTypeConfig{{Name: name, Mode: ModeSubagent, Tools: []string{"bash"}, Model: "model-b"}})

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

	set := NewSet([]AgentTypeConfig{{Name: name, Mode: ModeSubagent, Tools: []string{"*"}}})

	cfg, ok := set.Get(name)
	require.True(t, ok)
	assert.Equal(t, defaultSubagentMaxIterations, cfg.MaxIterations)
	assert.Contains(t, cfg.Tools, "-todoread")
	assert.Contains(t, cfg.Tools, "-todowrite")
}

// An agent file without a `tools:` key is the ecosystem's "inherit everything"
// shorthand; it must not resolve to a spawnable but toolless agent.
func TestNewSet_SubagentWithoutToolsInheritsFullInventory(t *testing.T) {
	name := AgentType("docs-writer")

	set := NewSet([]AgentTypeConfig{{Name: name, Mode: ModeSubagent}})

	all := []string{"read", "write", "bash", "todoread", "todowrite"}
	assert.Equal(t, []string{"read", "write", "bash"}, set.FilterTools(all, name))
}

func TestNewSet_SubagentWithExplicitlyEmptyToolsStaysToolless(t *testing.T) {
	name := AgentType("mute")

	set := NewSet([]AgentTypeConfig{{Name: name, Mode: ModeSubagent, Tools: []string{}}})

	assert.Empty(t, set.FilterTools([]string{"read", "write"}, name))
}

func TestNewSet_ProjectSubagentShadowsBuiltIn(t *testing.T) {
	set := NewSet([]AgentTypeConfig{{Name: AgentTypeGeneral, Mode: ModeSubagent, Description: "custom general"}})

	cfg, ok := set.Get(AgentTypeGeneral)
	require.True(t, ok)
	assert.Equal(t, "custom general", cfg.Description)
}

func TestSet_ListSubagentsDeterministic(t *testing.T) {
	set := NewSet([]AgentTypeConfig{
		{Name: "zeta", Mode: ModeSubagent},
		{Name: "alpha", Mode: ModeSubagent},
	})

	subagents := set.ListSubagents()
	names := make([]string, 0, len(subagents))
	for _, cfg := range subagents {
		names = append(names, string(cfg.Name))
	}

	// build (primary) and compaction (hidden) are excluded; result is name-sorted.
	assert.Equal(t, []string{"alpha", "explore", "general", "zeta"}, names)
}

func TestSet_FilterTools(t *testing.T) {
	set := NewSet(nil)
	all := []string{"read", "write", "todoread", "todowrite", "bash"}

	general := set.FilterTools(all, AgentTypeGeneral)
	assert.Contains(t, general, "read")
	assert.Contains(t, general, "write")
	assert.NotContains(t, general, "todoread")
	assert.NotContains(t, general, "todowrite")

	explore := set.FilterTools(all, AgentTypeExplore)
	assert.Contains(t, explore, "read")
	assert.NotContains(t, explore, "write")

	assert.Nil(t, set.FilterTools(all, AgentType("nonexistent")))
}
