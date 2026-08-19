package registry

import (
	"cmp"
	"slices"
)

const (
	AgentTypeBuild      AgentType = "build"
	AgentTypeGeneral    AgentType = "general"
	AgentTypeExplore    AgentType = "explore"
	AgentTypeCompaction AgentType = "compaction"
)

// Agent modes.
const (
	ModePrimary  = "primary"
	ModeSubagent = "subagent"
	ModeHidden   = "hidden"
)

const defaultSubagentMaxIterations = 50

var builtinAgentTypes = map[AgentType]AgentTypeConfig{
	AgentTypeBuild: {
		Name:          AgentTypeBuild,
		Description:   "Primary build agent with full tool access",
		Mode:          ModePrimary,
		Tools:         []string{"*"},
		Prompt:        BuildAgentPrompt,
		MaxIterations: 0, // unlimited
	},
	AgentTypeGeneral: {
		Name:          AgentTypeGeneral,
		Description:   "Full-capability subagent — can read, write, edit files, run commands, fetch URLs, and use all available tools. Use for any task that requires action or modification: implementation, testing, running commands, data processing, multi-step operations.",
		Mode:          ModeSubagent,
		Tools:         []string{"*", "-todoread", "-todowrite"},
		Prompt:        GeneralAgentPrompt,
		MaxIterations: 25,
	},
	AgentTypeExplore: {
		Name:          AgentTypeExplore,
		Description:   "Read-only research subagent — can read files, search code (grep/glob), list directories, and run read-only shell commands (git log, find, wc). Cannot modify files. Use for investigation: tracing code paths, understanding architecture, finding usages, answering questions about the codebase.",
		Mode:          ModeSubagent,
		Tools:         []string{"read", "grep", "glob", "ls", "bash"},
		Prompt:        ExploreAgentPrompt,
		MaxIterations: 10,
	},
	AgentTypeCompaction: {
		Name:          AgentTypeCompaction,
		Description:   "Context compression agent",
		Mode:          ModeHidden,
		Tools:         []string{},
		Prompt:        CompactionInitialPrompt,
		MaxIterations: 0, // unlimited
	},
}

// AgentType defines different agent configurations.
type (
	AgentType string

	// AgentTypeConfig contains the configuration for a specific agent type.
	AgentTypeConfig struct {
		Name          AgentType
		Description   string
		Mode          string   // "primary", "subagent", "hidden"
		Tools         []string // allowed tool IDs, "*" for all, "-toolname" to exclude
		Prompt        string
		Model         string // optional model override for this agent type
		MaxIterations int    // 0 = unlimited
	}
)

// Set is an immutable per-session agent-type catalog: built-ins overlaid with the
// session's project-local subagents. Safe for concurrent reads.
type Set struct {
	types map[AgentType]AgentTypeConfig
}

// NewSet seeds the built-in agent types and overlays the session's project-local
// subagents on top, normalizing each (subagent-mode todo exclusion, default
// MaxIterations). A project subagent may shadow a built-in of the same name.
func NewSet(projectSubagents []AgentTypeConfig) *Set {
	types := make(map[AgentType]AgentTypeConfig, len(builtinAgentTypes)+len(projectSubagents))

	for name, cfg := range builtinAgentTypes {
		types[name] = cloneConfig(cfg)
	}

	for _, cfg := range projectSubagents {
		cfg = cloneConfig(cfg)
		types[cfg.Name] = normalizeSubagent(cfg)
	}

	return &Set{types: types}
}

// Get returns the configuration for an agent type.
func (s *Set) Get(t AgentType) (AgentTypeConfig, bool) {
	cfg, ok := s.types[t]

	return cloneConfig(cfg), ok
}

// Has reports whether the set contains the given agent type.
func (s *Set) Has(t AgentType) bool {
	_, ok := s.types[t]

	return ok
}

// ListSubagents returns all subagent-mode configs in deterministic (name) order.
func (s *Set) ListSubagents() []AgentTypeConfig {
	out := make([]AgentTypeConfig, 0)

	for _, cfg := range s.types {
		if cfg.Mode == ModeSubagent {
			out = append(out, cloneConfig(cfg))
		}
	}

	slices.SortFunc(out, func(a, b AgentTypeConfig) int { return cmp.Compare(a.Name, b.Name) })

	return out
}

// FilterTools filters a tool list by the agent type's restrictions: "*" includes
// all, "-name" excludes, a bare name includes only that tool.
func (s *Set) FilterTools(allTools []string, t AgentType) []string {
	config, ok := s.types[t]
	if !ok {
		return nil
	}

	if len(config.Tools) == 0 {
		return []string{}
	}

	excludeSet := make(map[string]bool)
	includeAll := false
	includeSet := make(map[string]bool)

	for _, tl := range config.Tools {
		if tl == "*" {
			includeAll = true
		} else if tl != "" && tl[0] == '-' {
			excludeSet[tl[1:]] = true
		} else {
			includeSet[tl] = true
		}
	}

	var result []string

	for _, tl := range allTools {
		if excludeSet[tl] {
			continue
		}

		if includeAll || includeSet[tl] {
			result = append(result, tl)
		}
	}

	return result
}

func cloneConfig(config AgentTypeConfig) AgentTypeConfig {
	config.Tools = slices.Clone(config.Tools)

	return config
}

// normalizeSubagent applies subagent defaults: the iteration cap, an omitted tool
// list meaning "inherit everything", and todoread/todowrite left to the primary agent.
func normalizeSubagent(config AgentTypeConfig) AgentTypeConfig {
	if config.MaxIterations == 0 {
		config.MaxIterations = defaultSubagentMaxIterations
	}

	// nil is "no tools: key", []string{} is an author asking for nothing.
	if config.Tools == nil {
		config.Tools = []string{"*"}
	}

	if config.Mode == ModeSubagent {
		config.Tools = excludeTodoTools(config.Tools)
	}

	return config
}

// excludeTodoTools appends todo-tool exclusions when not already present, so
// subagents never see todoread/todowrite regardless of how Tools was specified.
func excludeTodoTools(tools []string) []string {
	for _, excl := range []string{"-todoread", "-todowrite"} {
		if !slices.Contains(tools, excl) {
			tools = append(tools, excl)
		}
	}

	return tools
}
