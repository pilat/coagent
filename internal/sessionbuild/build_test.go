package sessionbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

func TestBuildPrompt_AgentInventory(t *testing.T) {
	tests := []struct {
		name                      string
		agent                     registry.AgentType
		task                      bool
		wantSkills, wantSubagents bool
	}{
		{"build with owners", registry.AgentTypeBuild, true, true, true},
		{"build without task", registry.AgentTypeBuild, false, true, false},
		{"builtin explore", registry.AgentTypeExplore, true, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ldr := loader.New()
			ldr.RegisterSkill(&loader.Skill{Name: "review", Description: "Review changes"})
			ldr.RegisterSubagent(&loader.Subagent{Name: "custom", Description: "Custom helper"})
			reg := tool.NewRegistry()
			reg.Register(builtin.NewSkillTool(ldr))
			reg.Register(testTool{id: "read"})
			if tc.task {
				reg.Register(testTool{id: tool.IDTask})
			}
			set := registry.NewSet(nil)
			agent, ok := set.Get(tc.agent)
			require.True(t, ok)
			reg = filterRegistryForAgent(set, reg, agent)
			in := BuildInput{Config: &config.Config{Model: "current-model"}, Loader: ldr, WorkDir: t.TempDir()}
			prompt := preparePrompt(in, agent, nil, todo.New(), reg, nil)
			text := prompt.SystemPrompt()
			assert.Equal(t, tc.wantSkills, containsText(text, "## Available Skills"))
			assert.Equal(t, tc.wantSubagents, containsText(text, "## Available Subagents"))
			if tc.agent == registry.AgentTypeExplore {
				assert.NotContains(t, text, "Model: current-model")
			} else {
				assert.Contains(t, text, "Model: current-model")
			}
			assert.NotContains(t, text, "# Active background work")
		})
	}
}

func TestLoadOpeningContext_LeanAndProjectAgents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "AGENTS.md"), []byte("Project constraint."), 0o600))
	for _, tc := range []struct {
		name string
		omit bool
	}{{"project", false}, {"lean", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ldr := loader.New()
			skill := &loader.Skill{Name: "injected", Content: "Injected constraint."}
			in := BuildInput{
				Config:      &config.Config{},
				Record:      &sessionstore.SessionRecord{},
				Loader:      ldr,
				WorkDir:     workDir,
				ExtraSkills: []*loader.Skill{skill},
			}
			opening, skills := loadOpeningContext(
				t.Context(),
				in,
				registry.AgentTypeConfig{OmitProjectContext: tc.omit},
			)
			if tc.omit {
				assert.Empty(t, opening)
				assert.Empty(t, skills)
				assert.Nil(t, ldr.GetSkill("injected"))
			} else {
				assert.Contains(t, opening, "Project constraint.")
				assert.Equal(t, []*loader.Skill{skill}, skills)
				assert.NotNil(t, ldr.GetSkill("injected"))
			}
		})
	}
}

func TestTodoReplacement_StableIdentitiesAndTimestamps(t *testing.T) {
	memory := todo.New()
	memory.Add("stale", todo.PriorityHigh)
	replacement := &todoReplacement{memory: memory}
	items, err := replacement.ReplaceTodo(
		t.Context(),
		"call-1",
		[]todo.ReplacementItem{{Content: "first", Status: todo.StatusInProgress}, {Content: "second"}},
	)
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "call-1:0", items[0].ID)
	assert.Equal(t, "call-1:1", items[1].ID)
	for _, item := range items {
		assert.False(t, item.CreatedAt.IsZero())
		assert.False(t, item.UpdatedAt.IsZero())
		assert.NotNil(t, memory.Get(item.ID))
	}
	_, err = replacement.ReplaceTodo(t.Context(), "", []todo.ReplacementItem{{Content: "bad"}})
	require.Error(t, err)
	assert.Len(t, memory.List(), 2)
	_, err = replacement.ReplaceTodo(t.Context(), "call-2", nil)
	require.NoError(t, err)
	assert.Empty(t, memory.List())
}

func containsText(value, needle string) bool { return strings.Contains(value, needle) }
