package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/memory"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

// promptRecordingLLM captures the system prompt of every request it answers.
type promptRecordingLLM struct {
	mockLLMClient

	mu      sync.Mutex
	prompts []string
}

func (m *promptRecordingLLM) Chat(
	_ context.Context,
	system string,
	_ []llmwire.Message,
	_ []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	m.mu.Lock()
	m.prompts = append(m.prompts, system)
	m.mu.Unlock()

	return &llmwire.Response{Text: "done", FinishType: llmwire.FinishStop}, nil
}

func (m *promptRecordingLLM) firstPrompt(t *testing.T) string {
	t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()

	require.NotEmpty(t, m.prompts, "the session must have made at least one LLM call")

	return m.prompts[0]
}

// newPromptTestSession builds a session over workDir with the core builtin ids
// registered, returning it alongside the LLM that records its system prompts.
func newPromptTestSession(
	t *testing.T,
	workDir string,
	ldr loader.Service,
	agentType registry.AgentType,
	models ...config.ModelEntry,
) (*svc, *promptRecordingLLM) {
	t.Helper()

	reg := tool.NewRegistry()
	for _, id := range []string{"read", "write", "edit", "grep", "glob", "ls", "bash", tool.IDSkill} {
		reg.Register(testTool{id: id})
	}

	llmClient := &promptRecordingLLM{}
	var uc *config.UnifiedConfig
	if len(models) > 0 {
		uc = &config.UnifiedConfig{Models: models}
	}

	p := params{
		Config:    &config.Config{WorkDir: workDir, Model: "test-model", UnifiedConfig: uc},
		LLMClient: llmClient,
		TodoStore: todo.New(),
		Loader:    ldr,
		Registry:  reg,
	}

	sess, err := newWithOptions(context.Background(), p, options{ID: 1, AgentType: agentType})
	require.NoError(t, err)

	return sess.(*svc), llmClient
}

func TestSystemPrompt_DoesNotAdvertiseConfiguredModelCatalog(t *testing.T) {
	models := []config.ModelEntry{
		{ID: "test-model", Name: "Current", ContextWindow: 100_000},
		{ID: "hidden-model", Name: "Hidden", ContextWindow: 200_000},
		{ID: "tagged-model", Name: "Candidate", ContextWindow: 300_000, Tags: []string{"coding"}},
	}

	for _, tc := range []struct {
		name          string
		agentType     registry.AgentType
		withTask      bool
		wantModelName bool
	}{
		{name: "root with task", agentType: registry.AgentTypeBuild, withTask: true, wantModelName: true},
		{name: "lean explore without task", agentType: registry.AgentTypeExplore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, llmClient := newPromptTestSession(t, t.TempDir(), loader.New(), tc.agentType, models...)
			if tc.withTask {
				require.True(t, s.RegisterGatedTool(testTool{id: tool.IDTask}))
			}

			_, err := s.run(context.Background(), "do the thing")
			require.NoError(t, err)

			prompt := llmClient.firstPrompt(t)
			if tc.wantModelName {
				assert.Contains(t, prompt, "- Model: test-model")
			} else {
				assert.NotContains(t, prompt, "- Model: test-model")
			}
			assert.NotContains(t, prompt, "## Available Models")
			assert.NotContains(t, prompt, "hidden-model")
			assert.NotContains(t, prompt, "tagged-model")
			assert.NotContains(t, prompt, "100k context")
		})
	}
}

func TestBuiltInExplore_OmitsProjectContext(t *testing.T) {
	workDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "AGENTS.md"), []byte("PROJECT INSTRUCTIONS"), 0o600))

	reg := tool.NewRegistry()
	for _, id := range []string{"read", "write", "grep", "glob", "ls", "bash", tool.IDSkill} {
		reg.Register(testTool{id: id})
	}

	p := params{
		Config:      &config.Config{WorkDir: workDir, Model: "test-model"},
		LLMClient:   &promptRecordingLLM{},
		TodoStore:   todo.New(),
		Loader:      loader.New(),
		Registry:    reg,
		MemoryStore: &stubMemoryStore{entries: []memory.MemoryEntry{{ID: 1, Text: "PROJECT MEMORY"}}},
		GitClient: &fakeRepoStateClient{state: git.RepositoryState{
			Status: git.RepositoryAvailable, Branch: "main", Hash: "abc123",
		}},
	}

	service, err := newWithOptions(context.Background(), p, options{
		ID: 1, AgentType: registry.AgentTypeExplore, ProjectID: 1,
	})
	require.NoError(t, err)

	s := service.(*svc)
	system := s.prompt.systemPrompt()
	assert.True(t, strings.HasPrefix(system, registry.ExploreAgentPrompt))
	assert.Contains(t, system, "# Environment")
	assert.Contains(t, system, "# TOOLS")
	assert.NotContains(t, system, "PROJECT MEMORY")
	assert.NotContains(t, system, "- Model: test-model")
	assert.Empty(t, s.agentsMD)
	assert.Nil(t, s.registry.Get("memory_save"))
	assert.Nil(t, s.registry.Get("memory_delete"))
	assert.Equal(t, "task", s.appendGitStateDelta(context.Background(), "task"))
}

func TestSystemPromptExcludesVolatileOpeningAndBackgroundContext(t *testing.T) {
	workDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "AGENTS.md"), []byte("PROJECT INSTRUCTIONS"), 0o600))

	p := params{
		Config:      &config.Config{WorkDir: workDir, Model: "test-model"},
		LLMClient:   &promptRecordingLLM{},
		TodoStore:   todo.New(),
		Loader:      loader.New(),
		Registry:    tool.NewRegistry(),
		MemoryStore: &stubMemoryStore{entries: []memory.MemoryEntry{{ID: 7, Text: "PROJECT MEMORY"}}},
	}
	opts := options{
		ID: 1, ProjectID: 1,
		ActiveProcesses: []ActiveProcessInfo{{ID: "bgp_1", OutputPath: "/tmp/output"}},
	}
	service, err := newWithOptions(t.Context(), p, opts)
	require.NoError(t, err)

	s := service.(*svc)
	system := s.prompt.systemPrompt()
	assert.NotContains(t, system, "Date:")
	assert.NotContains(t, system, "Timezone:")
	assert.NotContains(t, system, "PROJECT MEMORY")
	assert.NotContains(t, system, "Active background work")

	opening := s.openingTurn("task")
	require.Len(t, opening, 2)
	assert.True(t, strings.HasPrefix(opening[0].Content, agentsMDMessagePrefix))
	assert.Contains(t, opening[0].Content, "PROJECT INSTRUCTIONS")
	assert.Contains(t, opening[0].Content, "- [7] PROJECT MEMORY")
	assert.Contains(t, buildActiveBackgroundSection(opts.ActiveProcesses, nil), "process bgp_1")
}

func TestOpeningTurnRecognizesMemoryOnlyProjectContext(t *testing.T) {
	s := &svc{agentsMD: buildMemoriesSection(t.Context(), &stubMemoryStore{entries: []memory.MemoryEntry{{
		ID: 3, Text: "memory only",
	}}}, 1)}

	opening := s.openingTurn("task")
	require.Len(t, opening, 2)
	assert.True(t, strings.HasPrefix(opening[0].Content, agentsMDMessagePrefix))
	assert.Contains(t, opening[0].Content, "- [3] memory only")
	assert.Equal(t, 2, compactionHeaderSize(opening))
}

func TestResumeKeepsPersistedOpeningContextAndStableSystemPrompt(t *testing.T) {
	workDir := t.TempDir()
	agentsPath := filepath.Join(workDir, "AGENTS.md")
	require.NoError(t, os.WriteFile(agentsPath, []byte("ORIGINAL RULES"), 0o600))
	memoryStore := &stubMemoryStore{entries: []memory.MemoryEntry{{ID: 1, Text: "original memory"}}}

	newParams := func() params {
		return params{
			Config:      &config.Config{WorkDir: workDir, Model: "test-model"},
			LLMClient:   &promptRecordingLLM{},
			TodoStore:   todo.New(),
			Loader:      loader.New(),
			Registry:    tool.NewRegistry(),
			MemoryStore: memoryStore,
		}
	}

	firstService, err := newWithOptions(t.Context(), newParams(), options{ID: 1, ProjectID: 1})
	require.NoError(t, err)
	first := firstService.(*svc)
	opening := first.openingTurn("original task")
	system := first.prompt.systemPrompt()

	require.NoError(t, os.WriteFile(agentsPath, []byte("CHANGED RULES"), 0o600))
	memoryStore.entries = []memory.MemoryEntry{{ID: 2, Text: "changed memory"}}
	resumedService, err := newWithOptions(t.Context(), newParams(), options{
		ID: 1, ProjectID: 1, ResumeMessages: opening,
	})
	require.NoError(t, err)
	resumed := resumedService.(*svc)

	assert.Equal(t, system, resumed.prompt.systemPrompt())
	assert.Equal(t, opening, resumed.ms.getMessages())
	assert.Contains(t, resumed.ms.getMessages()[0].Content, "ORIGINAL RULES")
	assert.Contains(t, resumed.ms.getMessages()[0].Content, "original memory")
	assert.NotContains(t, resumed.ms.getMessages()[0].Content, "CHANGED RULES")
	assert.NotContains(t, resumed.ms.getMessages()[0].Content, "changed memory")
}

func TestProjectExploreOverride_KeepsProjectContext(t *testing.T) {
	workDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "AGENTS.md"), []byte("PROJECT INSTRUCTIONS"), 0o600))
	writeProjectAgent(
		t,
		workDir,
		"explore",
		"---\nname: explore\ndescription: Custom explorer\n---\nCUSTOM EXPLORE PROMPT\n",
	)

	s, _ := newPromptTestSession(t, workDir, loader.New(), registry.AgentTypeExplore)

	assert.Contains(t, s.prompt.systemPrompt(), "CUSTOM EXPLORE PROMPT")
	assert.Contains(t, s.prompt.systemPrompt(), "- Model: test-model")
	assert.Equal(t, "PROJECT INSTRUCTIONS", s.agentsMD)
}

func writeProjectAgent(t *testing.T, workDir, name, body string) {
	t.Helper()

	dir := filepath.Join(workDir, ".claude", "agents")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".md"), []byte(body), 0o600))
}

// The daemon registers task/schedule/sleep onto the live registry only after the
// session object exists, so a prompt frozen at construction advertises neither.
func TestSystemPrompt_AdvertisesToolsRegisteredAfterConstruction(t *testing.T) {
	s, llmClient := newPromptTestSession(t, t.TempDir(), loader.New(), registry.AgentTypeBuild)

	for _, id := range []string{tool.IDTask, tool.IDSchedule, tool.IDSleep} {
		require.True(t, s.RegisterGatedTool(testTool{id: id}), "daemon tool %q must pass the gate", id)
	}

	_, err := s.run(context.Background(), "do the thing")
	require.NoError(t, err)

	prompt := llmClient.firstPrompt(t)
	assert.Contains(t, prompt, "Sub-agents: task")
	assert.Contains(t, prompt, "Scheduling: schedule")
	assert.Contains(t, prompt, "# SCHEDULING")
	assert.Contains(t, prompt, "Never use sleep, schedule, or get_subagent_result polling to wait for subagents")
}

// The subagents inventory tells the model to use the 'task' tool; it must be
// gated on that tool exactly like the skills inventory is gated on 'skill'.
func TestSystemPrompt_AnnouncesSubagentsOnlyWhenTaskToolIsAvailable(t *testing.T) {
	for _, tc := range []struct {
		name         string
		agentType    registry.AgentType
		wantAnnounce bool
	}{
		{name: "build with task", agentType: registry.AgentTypeBuild, wantAnnounce: true},
		{name: "explore without task", agentType: registry.AgentTypeExplore, wantAnnounce: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			writeProjectAgent(
				t,
				workDir,
				"reviewer",
				"---\nname: reviewer\ndescription: Reviews changes\n---\nReview.\n",
			)

			s, llmClient := newPromptTestSession(t, workDir, loader.New(), tc.agentType)
			s.RegisterGatedTool(testTool{id: tool.IDTask})

			_, err := s.run(context.Background(), "do the thing")
			require.NoError(t, err)

			prompt := llmClient.firstPrompt(t)
			if tc.wantAnnounce {
				assert.Contains(t, prompt, "## Available Subagents")
				assert.Contains(t, prompt, "**reviewer**")
			} else {
				assert.NotContains(t, prompt, "## Available Subagents")
			}
		})
	}
}

// An agent file with no `tools:` key is copied straight from the wider ecosystem,
// where it means "inherit everything" — never "no capabilities at all".
func TestNewWithOptions_ProjectSubagentWithoutToolsKeyIsUsable(t *testing.T) {
	workDir := t.TempDir()
	writeProjectAgent(t, workDir, "docs-writer", "---\nname: docs-writer\ndescription: Writes docs\n---\nWrite docs.\n")

	s, _ := newPromptTestSession(t, workDir, loader.New(), registry.AgentType("docs-writer"))

	assert.NotNil(t, s.registry.Get("read"), "an inherited inventory keeps read")
	assert.NotNil(t, s.registry.Get("write"), "an inherited inventory keeps write")
	assert.Nil(t, s.registry.Get("todoread"), "todo tools stay excluded for subagents")
}
