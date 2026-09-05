package session

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/memory"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

func TestBuildToolsSection_TypicalSet(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&stubTool{id: "read"})
	reg.Register(&stubTool{id: "write"})
	reg.Register(&stubTool{id: "bash"})
	reg.Register(&stubTool{id: "memory_save"})
	reg.Register(&stubTool{id: "memory_delete"})
	reg.Register(&stubTool{id: "batch"})

	result := buildToolsSection(reg, false)

	assert.Contains(t, result, "File operations: read (view file), write (create/overwrite)")
	assert.Contains(t, result, "Shell: bash")
	assert.Contains(t, result, "# PERSISTENT MEMORY")
	assert.Contains(t, result, "Curated memories")
	assert.Contains(t, result, "# PARALLEL EXECUTION")
	assert.NotContains(t, result, "Scheduling")
	assert.NotContains(t, result, "lsp")
}

func TestBuildToolsSection_CuratedOnly(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&stubTool{id: "read"})
	reg.Register(&stubTool{id: "memory_save"})
	reg.Register(&stubTool{id: "memory_delete"})

	result := buildToolsSection(reg, false)

	assert.Contains(t, result, "# PERSISTENT MEMORY")
	assert.Contains(t, result, "Curated memories")
	assert.NotContains(t, result, "Session extractions")
}

func TestBuildToolsSection_Deterministic(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&stubTool{id: "read"})
	reg.Register(&stubTool{id: "write"})
	reg.Register(&stubTool{id: "bash"})
	reg.Register(&stubTool{id: "grep"})
	reg.Register(&stubTool{id: "glob"})
	reg.Register(&stubTool{id: "schedule"})
	reg.Register(&stubTool{id: "sleep"})

	first := buildToolsSection(reg, false)
	second := buildToolsSection(reg, false)

	require.Equal(t, first, second, "buildToolsSection must be deterministic")
}

func TestBuildToolsSection_ScheduleOnly(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&stubTool{id: "schedule"})

	result := buildToolsSection(reg, false)

	assert.Contains(t, result, "# SCHEDULING")
	assert.Contains(t, result, "Use schedule to set a wake-up timer")
	assert.NotContains(t, result, "sleep")
}

func TestBuildToolsSection_SleepOnly(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&stubTool{id: "sleep"})

	result := buildToolsSection(reg, false)

	assert.Contains(t, result, "# SCHEDULING")
	assert.Contains(t, result, "Use sleep to pause execution")
}

func TestBuildToolsSection_BothScheduleAndSleep(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&stubTool{id: "schedule"})
	reg.Register(&stubTool{id: "sleep"})

	result := buildToolsSection(reg, false)

	assert.Contains(t, result, "Prefer schedule over sleep")
}

func TestBuildToolsSection_SubagentsMustNotUseSleepOrPollingToWait(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&stubTool{id: "task"})
	reg.Register(&stubTool{id: "get_subagent_result"})
	reg.Register(&stubTool{id: "schedule"})
	reg.Register(&stubTool{id: "sleep"})

	result := buildToolsSection(reg, false)

	assert.Contains(t, result, "A sleep call cannot order or delay another tool call from the same assistant response")
	assert.Contains(t, result, "calls emitted together may execute concurrently")
	assert.Contains(t, result, "Never use sleep, schedule, or get_subagent_result polling to wait for subagents")
	assert.Contains(t, result, "Use foreground task when you need the answer now")
	assert.Contains(t, result, "background task completion is delivered automatically and wakes this session")
}

func TestBuildActiveSubagentsSection_TeachesAutomaticWakeNotPolling(t *testing.T) {
	result := buildActiveSubagentsSection([]ActiveSubagentInfo{{
		ChildID:  42,
		Blocking: false,
		State:    "running",
	}})

	assert.Contains(t, result, "Each completion is delivered automatically as a subagent_event and wakes this session")
	assert.Contains(t, result, "Do not wait with sleep or poll get_subagent_result")
	assert.Contains(t, result, "only a diagnostic snapshot")
	assert.NotContains(t, result, "poll status with")
}

func TestBuildToolsSection_WebSearchGuidance_Tavily(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&stubTool{id: "read"})
	reg.Register(&stubTool{id: "webfetch"})
	reg.Register(&stubTool{id: "mcp__tavily__tavily_search"})
	reg.Register(&stubTool{id: "mcp__tavily__tavily_extract"})

	result := buildToolsSection(reg, false)

	assert.Contains(t, result, "# WEB SEARCH")
	assert.Contains(t, result, "mcp__tavily__tavily_search")
	assert.NotContains(t, result, "mcp__tavily__tavily_extract") // extract is not a search tool
	assert.Contains(t, result, "Sources:")
	assert.Contains(t, result, "webfetch")
}

func TestBuildToolsSection_WebSearchGuidance_NoSearchMCP(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&stubTool{id: "read"})
	reg.Register(&stubTool{id: "webfetch"})
	reg.Register(&stubTool{id: "mcp__context7__query-docs"})

	result := buildToolsSection(reg, false)

	assert.NotContains(t, result, "# WEB SEARCH")
}

func TestBuildToolsSection_WebSearchGuidance_BraveSearch(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&stubTool{id: "mcp__brave-search__brave_web_search"})

	result := buildToolsSection(reg, false)

	assert.Contains(t, result, "# WEB SEARCH")
	assert.Contains(t, result, "mcp__brave-search__brave_web_search")
}

func TestBuildToolsSection_EmptyRegistry(t *testing.T) {
	reg := tool.NewRegistry()

	result := buildToolsSection(reg, false)

	assert.Contains(t, result, "# TOOLS")
	assert.NotContains(t, result, "# PERSISTENT MEMORY")
	assert.NotContains(t, result, "# PARALLEL EXECUTION")
	assert.NotContains(t, result, "# SCHEDULING")
}

// stubMemoryStore serves the prompt builder's only read.
type stubMemoryStore struct {
	memory.CuratedStore

	entries []memory.MemoryEntry
}

func (s *stubMemoryStore) ListMemoryTexts(context.Context, int64) ([]memory.MemoryEntry, error) {
	return s.entries, nil
}

// memory_delete takes an id, so the inventory the model reasons over has to
// carry one — otherwise the only way to call the tool is to guess.
func TestBuildMemoriesSectionRendersIDsTheDeleteToolNeeds(t *testing.T) {
	store := &stubMemoryStore{entries: []memory.MemoryEntry{
		{ID: 3, Text: "prefers tabs"},
		{ID: 7, Text: "deploys on Fridays"},
	}}

	result := buildMemoriesSection(t.Context(), store, 1)

	assert.Contains(t, result, "- [3] prefers tabs")
	assert.Contains(t, result, "- [7] deploys on Fridays")
}

func TestBuildSkillsSectionUsesModelVisibilityAndBoundedDescriptions(t *testing.T) {
	userDisabled := false
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{Name: "alpha", Description: strings.Repeat("界", 1537)})
	ldr.RegisterSkill(&loader.Skill{Name: "beta", UserInvocable: &userDisabled, Description: "model only"})
	ldr.RegisterSkill(&loader.Skill{Name: "gamma", DisableModelInvocation: true, Description: "user only"})

	result := buildSkillsSection(ldr)

	assert.Less(t, strings.Index(result, "**alpha**"), strings.Index(result, "**beta**"))
	assert.Contains(t, result, "**beta**: model only")
	assert.NotContains(t, result, "gamma")

	alphaLine, _, _ := strings.Cut(strings.Split(result, "**alpha**: ")[1], "\n")
	assert.Equal(t, 1536, utf8.RuneCountInString(alphaLine))
	assert.True(t, utf8.ValidString(alphaLine))
}

func TestSetupRegistryAnnouncesSkillsOnlyWhenToolIsAvailable(t *testing.T) {
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{Name: "review", Description: "Review changes"})

	for _, tc := range []struct {
		name      string
		agentType registry.AgentType
		wantList  bool
	}{
		{name: "build", agentType: registry.AgentTypeBuild, wantList: true},
		{name: "explore", agentType: registry.AgentTypeExplore, wantList: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := tool.NewRegistry()
			reg.Register(builtin.NewSkillTool(ldr))
			set := registry.NewSet(nil)
			config, ok := set.Get(tc.agentType)
			require.True(t, ok)

			s := &svc{
				agentTypes: set,
				loader:     ldr,
				prompt:     newPromptBuilder("base", "", ""),
			}
			s.setupRegistry(params{Registry: reg}, config)

			if tc.wantList {
				assert.Contains(t, s.prompt.systemPrompt(), "## Available Skills")
			} else {
				assert.NotContains(t, s.prompt.systemPrompt(), "## Available Skills")
			}
		})
	}
}
