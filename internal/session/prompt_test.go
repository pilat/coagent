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
	reg.Register(&stubTool{id: "send_to_subagent"})
	reg.Register(&stubTool{id: "get_subagent_result"})

	result := buildToolsSection(reg, false)

	assert.Contains(t, result, "File operations: read (view file), write (create/overwrite)")
	assert.Contains(t, result, "Shell: bash")
	assert.Contains(t, result, "# PERSISTENT MEMORY")
	assert.Contains(t, result, "Curated memories")
	assert.Contains(t, result, "# PARALLEL EXECUTION")
	assert.Contains(
		t,
		result,
		"Sub-agents: send_to_subagent (continue/resume an existing subagent), get_subagent_result (diagnostic snapshot)",
	)
	assert.NotContains(t, result, "task (start a subagent assignment)")
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
	assert.Contains(t, result, "Use schedule only for recurring future work or reminders")
	assert.Contains(t, result, "Never use it to poll the status or completion of already-started work")
	assert.NotContains(t, result, "waiting for an external process")
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

	assert.Contains(t, result, "Use schedule only for recurring future work or reminders")
	assert.Contains(t, result, "Never use schedule to poll the status or completion of already-started work")
	assert.Contains(t, result, "Use sleep for a one-time fixed delay")
	assert.NotContains(t, result, "waiting for an external process")
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
	assert.Contains(t, result, "background task result arrives automatically in a new turn")
}

func TestBuildActiveBackgroundSection_TeachesAutomaticWakeNotPolling(t *testing.T) {
	result := buildActiveBackgroundSection([]ActiveProcessInfo{{
		ID: "bgp_1", OutputPath: "/tmp/process.out",
	}}, []ActiveSubagentInfo{{
		ChildID:  42,
		Blocking: false,
		State:    "running",
	}})

	assert.Contains(t, result, "# Active background work")
	assert.Contains(t, result, "process bgp_1 (running): output /tmp/process.out")
	assert.Contains(t, result, "#42 (background): running")
	assert.Contains(t, result, "result arrives automatically in a later turn")
	assert.Contains(t, result, "do not use timers to poll")
	assert.Contains(t, result, "Snapshot from activation start")
	assert.Contains(t, result, "Later Bash, task, cancellation, and completion observations take precedence")
	assert.Contains(t, result, "end the response")
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
	assert.NotContains(t, result, "Sub-agents:")
	assert.NotContains(t, result, "# UNTRUSTED CONTENT")
}

func TestBuildToolsSection_UntrustedContentGuidance(t *testing.T) {
	tests := []struct {
		name         string
		tools        []string
		nativeSearch bool
		want         bool
	}{
		{
			name:  "bash only session still gets the guidance",
			tools: []string{"bash"},
			want:  true,
		},
		{
			name:  "webfetch only",
			tools: []string{"webfetch"},
			want:  true,
		},
		{
			name:  "websearch only",
			tools: []string{"websearch"},
			want:  true,
		},
		{
			name:  "non-search mcp session",
			tools: []string{"mcp__context7__query-docs"},
			want:  true,
		},
		{
			name:         "usable native search",
			tools:        []string{"read"},
			nativeSearch: true,
			want:         true,
		},
		{
			name:         "native search without any client-side tool stays silent",
			tools:        nil,
			nativeSearch: true,
			want:         false,
		},
		{
			name:  "no relevant source",
			tools: []string{"read", "write"},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := tool.NewRegistry()
			for _, id := range tt.tools {
				reg.Register(&stubTool{id: id})
			}

			result := buildToolsSection(reg, tt.nativeSearch)

			if tt.want {
				assert.Contains(t, result, "# UNTRUSTED CONTENT")
				assert.Contains(t, result, "curl")
				assert.Contains(t, result, "wget")
				assert.Contains(t, result, "<<<BEGIN_UNTRUSTED_EXTERNAL_DATA id=\"...\">>>")
				assert.Contains(t, result, "<<<END_UNTRUSTED_EXTERNAL_DATA id=\"...\">>>")
				assert.Contains(t, result, "independently validated")
				assert.Contains(t, result, "may also contain ordinary local",
					"mixed batches wrap trusted fragments too; the guidance must say so")
				assert.NotContains(t, result, "must be ignored")
			} else {
				assert.NotContains(t, result, "# UNTRUSTED CONTENT")
			}
		})
	}
}

// The native-search advertisement must never claim search for a request the
// driver would not inject it into: a toolless registry stays silent even when
// the model triplet supports native search.
func TestBuildToolsSection_NativeSearchRequiresAClientSideTool(t *testing.T) {
	empty := buildToolsSection(tool.NewRegistry(), true)
	assert.NotContains(t, empty, "# WEB SEARCH")
	assert.NotContains(t, empty, "# UNTRUSTED CONTENT")

	populated := tool.NewRegistry()
	populated.Register(&stubTool{id: "read"})
	withTool := buildToolsSection(populated, true)
	assert.Contains(t, withTool, "# WEB SEARCH")
	assert.Contains(t, withTool, "provided natively by your model provider")
	assert.Contains(t, withTool, "# UNTRUSTED CONTENT")
}

func TestBuildToolsSection_UntrustedGuidanceIsDeterministic(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&stubTool{id: "bash"})
	reg.Register(&stubTool{id: "webfetch"})

	first := buildToolsSection(reg, false)
	second := buildToolsSection(reg, false)

	require.Equal(t, first, second)
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
				prompt:     newPromptBuilder("base", ""),
			}
			s.setupRegistry(params{Registry: reg}, config)
			s.refreshRegistrySections()

			if tc.wantList {
				assert.Contains(t, s.prompt.systemPrompt(), "## Available Skills")
			} else {
				assert.NotContains(t, s.prompt.systemPrompt(), "## Available Skills")
			}
		})
	}
}
