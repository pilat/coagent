package session

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/memory"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// Tool-name constants for the dynamic tool inventory rendered in the system prompt.
const (
	batchToolName = "batch"
	readToolName  = "read"
	writeToolName = "write"
	editToolName  = "edit"
	grepToolName  = "grep"
	globToolName  = "glob"
)

// toolCategories defines the order and grouping for the dynamic tool inventory.
var toolCategories = []struct {
	Name  string
	Tools []string
}{
	{
		"File operations",
		[]string{readToolName, writeToolName, editToolName, "apply_patch", globToolName, grepToolName, "ls"},
	},
	{"Shell", []string{"bash"}},
	{"Code intelligence", []string{"lsp"}},
	{"Task tracking", []string{"todoread", "todowrite"}},
	{"Memory", []string{"memory_save", "memory_delete"}},
	{"Sub-agents", []string{"task"}},
	{"Parallel execution", []string{batchToolName}},
	{"Skills", []string{"skill"}},
	{"Web", []string{"webfetch"}},
	{"Context management", []string{"compact_context"}},
	{"Scheduling", []string{"schedule", "sleep"}},
}

var toolDescriptions = map[string]string{
	readToolName:      "view file",
	writeToolName:     "create/overwrite",
	editToolName:      "find-replace",
	"apply_patch":     "unified diff",
	globToolName:      "find by pattern",
	grepToolName:      "search contents",
	"ls":              "list directory",
	"lsp":             "definitions, references, diagnostics",
	"task":            "spawn/resume subagents",
	batchToolName:     "run independent tools simultaneously",
	"skill":           "load domain knowledge",
	"webfetch":        "fetch known URL",
	"compact_context": "compress conversation history",
	"schedule":        "wake-up timer",
	"sleep":           "fixed delay",
}

// knownSearchMCPs lists MCP server config keys known to provide web search.
// When a registered tool ID starts with "mcp__{key}__" and contains "search",
// web search guidance is emitted in the dynamic prompt.
var knownSearchMCPs = []string{
	"tavily",
	"brave-search",
	"exa",
	"kagi",
	"searxng",
	"duckduckgo",
	"perplexity",
	"firecrawl",
}

// promptBuilder encapsulates the system prompt assembly: static base + dynamic sections.
// Thread-safe — the agent loop reads systemPrompt() while model switches and memory refreshes write.
type promptBuilder struct {
	mu                     sync.RWMutex
	basePrompt             string
	activeSkillsSection    string
	toolsSection           string
	skillsSection          string
	subagentsSection       string
	memoriesSection        string
	modelsSection          string
	activeSubagentsSection string
}

func newPromptBuilder(
	basePrompt, memoriesSection, modelsSection string,
	activeSkills ...*loader.Skill,
) *promptBuilder {
	return &promptBuilder{
		basePrompt:          basePrompt,
		activeSkillsSection: buildActiveSkillsSection(activeSkills),
		memoriesSection:     memoriesSection,
		modelsSection:       modelsSection,
	}
}

// systemPrompt returns the full system prompt, combining static base with dynamic sections.
// Safe to call from any goroutine — used as a callback by the agent loop.
func (p *promptBuilder) systemPrompt() string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.basePrompt + p.activeSkillsSection + p.toolsSection + p.skillsSection + p.subagentsSection +
		p.memoriesSection + p.modelsSection + p.activeSubagentsSection
}

// buildActiveSkillsSection embeds daemon-selected instructions directly in the
// system prompt. Unlike the skills inventory, these require no model tool call.
func buildActiveSkillsSection(skills []*loader.Skill) string {
	if len(skills) == 0 {
		return ""
	}

	var section strings.Builder
	section.WriteString(
		"\n\n# ACTIVE SKILLS\n\nThe following skill instructions are already active. Follow them directly; do not load them again.\n\n",
	)

	for i, skill := range skills {
		if i > 0 {
			section.WriteString("\n\n")
		}

		section.WriteString(builtin.RenderSkill(skill, ""))
	}

	return section.String()
}

// setActiveSubagentsSection replaces the pinned "# Active subagents" section.
// Refreshed on session create/resume from the durable subagent_links rows.
func (p *promptBuilder) setActiveSubagentsSection(section string) {
	p.mu.Lock()
	p.activeSubagentsSection = section
	p.mu.Unlock()
}

// setToolsSection replaces the tools section of the system prompt.
// Called after tool registration is complete.
func (p *promptBuilder) setToolsSection(section string) {
	p.mu.Lock()
	p.toolsSection = section
	p.mu.Unlock()
}

// setSkillsSection replaces the model-invocable skills inventory.
func (p *promptBuilder) setSkillsSection(section string) {
	p.mu.Lock()
	p.skillsSection = section
	p.mu.Unlock()
}

// setSubagentsSection replaces the spawnable-subagents inventory.
func (p *promptBuilder) setSubagentsSection(section string) {
	p.mu.Lock()
	p.subagentsSection = section
	p.mu.Unlock()
}

// buildToolsSection generates the dynamic TOOLS section from actual registered tool IDs.
func buildToolsSection(reg tool.Registry) string {
	ids := reg.IDs()
	registered := make(map[string]bool, len(ids))

	for _, id := range ids {
		registered[id] = true
	}

	var sb strings.Builder
	sb.WriteString("\n\n# TOOLS\n\nAvailable tools for this session:\n")

	for _, cat := range toolCategories {
		var tools []string

		for _, t := range cat.Tools {
			if !registered[t] {
				continue
			}

			if desc, ok := toolDescriptions[t]; ok {
				tools = append(tools, t+" ("+desc+")")
			} else {
				tools = append(tools, t)
			}
		}

		if len(tools) > 0 {
			sb.WriteString("- " + cat.Name + ": " + strings.Join(tools, ", ") + "\n")
		}
	}

	appendMemorySection(&sb, registered)
	appendParallelSection(&sb, registered)
	appendScheduleSection(&sb, registered)
	appendWebSearchSection(&sb, ids, registered)

	return sb.String()
}

func appendMemorySection(sb *strings.Builder, registered map[string]bool) {
	if !registered["memory_save"] && !registered["memory_delete"] {
		return
	}

	sb.WriteString("\n# PERSISTENT MEMORY\n\n")
	sb.WriteString(
		"**Curated memories** — shown in your system prompt above. Use memory_save to store short facts (max 200 chars, 50 per project). Use memory_delete to remove by ID. When the limit is reached, consolidate: merge related entries, delete obsolete ones. Always ask the user before modifying or deleting existing memories.\n",
	)
}

func appendParallelSection(sb *strings.Builder, registered map[string]bool) {
	if !registered[batchToolName] {
		return
	}

	sb.WriteString("\n# PARALLEL EXECUTION\n\n")
	sb.WriteString(
		"Use the `batch` tool to run independent tool calls simultaneously — reading multiple files, running grep + glob, combining unrelated commands. Do not batch operations where one depends on another's output.\n",
	)
}

func appendScheduleSection(sb *strings.Builder, registered map[string]bool) {
	hasSchedule := registered["schedule"]
	hasSleep := registered["sleep"]

	if !hasSchedule && !hasSleep {
		return
	}

	sb.WriteString("\n# SCHEDULING\n\n")

	switch {
	case hasSchedule && hasSleep:
		sb.WriteString(
			"Use schedule to set a wake-up timer when waiting for an external process (build, deploy, long test run). Use sleep for short fixed delays. Prefer schedule over sleep for longer waits — it frees resources.\n",
		)
	case hasSchedule:
		sb.WriteString(
			"Use schedule to set a wake-up timer when waiting for an external process (build, deploy, long test run).\n",
		)
	default:
		sb.WriteString("Use sleep to pause execution for a specified duration.\n")
	}

	if hasSleep {
		sb.WriteString(
			"A sleep call cannot order or delay another tool call from the same assistant response; calls emitted together may execute concurrently.\n",
		)
	}

	if registered[tool.IDTask] {
		sb.WriteString(
			"Never use sleep, schedule, or get_subagent_result polling to wait for subagents. Use foreground task when you need the answer now; background task completion is delivered automatically and wakes this session.\n",
		)
	}
}

func appendWebSearchSection(sb *strings.Builder, ids []string, registered map[string]bool) {
	var searchTools []string

	for _, key := range knownSearchMCPs {
		prefix := "mcp__" + key + "__"
		for _, id := range ids {
			if strings.HasPrefix(id, prefix) && strings.Contains(id, "search") {
				searchTools = append(searchTools, id)
			}
		}
	}

	if len(searchTools) == 0 {
		return
	}

	sb.WriteString("\n# WEB SEARCH\n\n")
	sb.WriteString("You have web search capability via: ")
	sb.WriteString(strings.Join(searchTools, ", "))
	sb.WriteString("\n\nUse web search for:\n")
	sb.WriteString("- Information beyond your knowledge cutoff\n")
	sb.WriteString("- Current documentation, recent changes, latest versions\n")

	if registered["webfetch"] {
		sb.WriteString("- Finding URLs before fetching them with webfetch\n\n")
		sb.WriteString("Do not guess URLs — search first, then use webfetch for the found URL.\n")
	} else {
		sb.WriteString("\n")
	}

	sb.WriteString(
		"After using search results in your response, include a Sources: section with the URLs you used.\n",
	)
}

// refreshMemories reloads curated memories from the store and rebuilds the memoriesSection.
// Called after memory_save/memory_delete.
func (p *promptBuilder) refreshMemories(ctx context.Context, store memory.CuratedStore, projectID int64) {
	if store == nil {
		return
	}

	section := buildMemoriesSection(ctx, store, projectID)

	p.mu.Lock()
	p.memoriesSection = section
	p.mu.Unlock()
}

// setModelsSection replaces the models section of the system prompt.
// Called after model switch.
func (p *promptBuilder) setModelsSection(section string) {
	p.mu.Lock()
	p.modelsSection = section
	p.mu.Unlock()
}

// buildMemoriesSection formats curated memories for the system prompt.
func buildMemoriesSection(ctx context.Context, store memory.CuratedStore, projectID int64) string {
	memories, err := store.ListMemoryTexts(ctx, projectID)
	if err != nil || len(memories) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n# YOUR MEMORIES\nPer-project memories. Manage with memory_save / memory_delete.\n\n")

	// The id is the only handle memory_delete accepts; without it the model guesses.
	for _, m := range memories {
		fmt.Fprintf(&sb, "- [%d] %s\n", m.ID, m.Text)
	}

	return sb.String()
}

// buildModelsSection records the inherited model without duplicating task's
// tagged-candidate policy into the general system prompt.
func buildModelsSection(currentModel string) string {
	return "\n- Model: " + currentModel
}

// buildSkillsSection returns the skills block for the system prompt.
// Lists model-invocable skills loaded by the loader.
func buildSkillsSection(ldr loader.Registry) string {
	skills := ldr.ListModelInvocableSkills()

	if len(skills) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString("\n\n## Available Skills\nYou can invoke these skills using the 'skill' tool:\n")

	for _, sk := range skills {
		fmt.Fprintf(&b, "- **%s**", sk.Name)

		if description := sk.AnnouncementDescription(); description != "" {
			fmt.Fprintf(&b, ": %s", description)
		}

		b.WriteString("\n")
	}

	return b.String()
}

// buildActiveSubagentsSection lists the parent's in-flight / awaiting-delivery
// children (pushed by the daemon) so a "subagent N finished" event never
// references an unknown N (the spawning task result may be compacted).
func buildActiveSubagentsSection(links []ActiveSubagentInfo) string {
	if len(links) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\n# Active subagents\n")
	b.WriteString(
		"Subagents you spawned that are still running or awaiting result delivery. " +
			"Each completion is delivered automatically as a subagent_event and wakes this session. " +
			"Do not wait with sleep or poll get_subagent_result; that tool is only a diagnostic snapshot.\n",
	)

	for _, l := range links {
		kind := "background"
		if l.Blocking {
			kind = "blocking"
		}

		fmt.Fprintf(&b, "- #%d (%s): %s\n", l.ChildID, kind, l.State)
	}

	return b.String()
}

// buildSubagentsSection returns the subagents block for the system prompt.
// Lists subagent definitions loaded by the loader.
func buildSubagentsSection(ldr loader.Registry) string {
	subagents := ldr.ListSubagents()
	if len(subagents) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n## Available Subagents\nYou can spawn these subagents using the 'task' tool:\n")

	for _, sa := range subagents {
		fmt.Fprintf(&b, "- **%s**", sa.Name)

		if sa.Description != "" {
			fmt.Fprintf(&b, ": %s", sa.Description)
		}

		b.WriteString("\n")
	}

	return b.String()
}
