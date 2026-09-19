//go:build linux

package loader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/safefile"
)

func TestNew(t *testing.T) {
	service := New()
	require.NotNil(t, service)

	_ = service

	s, ok := service.(*svc)
	require.True(t, ok)
	assert.NotNil(t, s.skills)
	assert.NotNil(t, s.subagents)
	assert.Empty(t, s.skills)
	assert.Empty(t, s.subagents)
}

func TestLoadSkills(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	skillsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.SkillsDirName)
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))

	codingSkillDir := filepath.Join(skillsDir, "coding")
	require.NoError(t, os.MkdirAll(codingSkillDir, 0o755))
	codingSkillContent := `# Coding Skill

This skill helps with coding tasks.

## Usage

Use this skill when writing code.`
	require.NoError(t, os.WriteFile(
		filepath.Join(codingSkillDir, config.SkillFileName),
		[]byte(codingSkillContent),
		0o644,
	))

	debugSkillContent := `# Debug Skill

This skill helps with debugging.`
	require.NoError(t, os.WriteFile(
		filepath.Join(skillsDir, "debug.md"),
		[]byte(debugSkillContent),
		0o644,
	))

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)
	skills := svc.ListSkills()
	assert.Len(t, skills, 2)

	coding := svc.GetSkill("coding")
	require.NotNil(t, coding)
	assert.Equal(t, "coding", coding.Name)
	assert.Contains(t, coding.Content, "Coding Skill")

	debug := svc.GetSkill("debug")
	require.NotNil(t, debug)
	assert.Equal(t, "debug", debug.Name)
	assert.Contains(t, debug.Content, "Debug Skill")
}

func TestLoadSkills_WithFrontmatter(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	skillsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.SkillsDirName)
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))

	testSkillDir := filepath.Join(skillsDir, "test-skill")
	require.NoError(t, os.MkdirAll(testSkillDir, 0o755))
	skillContent := `---
name: advanced-coding
description: Advanced coding techniques for Go
disable-model-invocation: true
---

# Advanced Coding

This skill provides advanced coding patterns.

## Patterns

- Interface-first design
- Dependency injection`
	require.NoError(t, os.WriteFile(
		filepath.Join(testSkillDir, config.SkillFileName),
		[]byte(skillContent),
		0o644,
	))

	minimalContent := `---
name: minimal-skill
description: A minimal skill example
---

Minimal content here.`
	require.NoError(t, os.WriteFile(
		filepath.Join(skillsDir, "minimal.md"),
		[]byte(minimalContent),
		0o644,
	))

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)
	skills := svc.ListSkills()
	assert.Len(t, skills, 2)

	advanced := svc.GetSkill("advanced-coding")
	require.NotNil(t, advanced)
	assert.Equal(t, "advanced-coding", advanced.Name)
	assert.Equal(t, "Advanced coding techniques for Go", advanced.Description)
	assert.False(t, advanced.IsModelInvocable())
	assert.Contains(t, advanced.Content, "Interface-first design")

	minimal := svc.GetSkill("minimal-skill")
	require.NotNil(t, minimal)
	assert.Equal(t, "minimal-skill", minimal.Name)
	assert.Equal(t, "A minimal skill example", minimal.Description)
}

func TestLoadSkills_NoSkillsDir(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)
	skills := svc.ListSkills()
	assert.Empty(t, skills)
}

func TestLoadSkills_EmptyDir(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	skillsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.SkillsDirName)
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)
	skills := svc.ListSkills()
	assert.Empty(t, skills)
}

func TestLoadSkills_InvalidFrontmatter(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	skillsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.SkillsDirName)
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))

	validSkillDir := filepath.Join(skillsDir, "valid")
	require.NoError(t, os.MkdirAll(validSkillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(validSkillDir, config.SkillFileName),
		[]byte("Valid content"),
		0o644,
	))

	invalidSkillDir := filepath.Join(skillsDir, "invalid")
	require.NoError(t, os.MkdirAll(invalidSkillDir, 0o755))
	invalidContent := `---
name: [invalid: yaml: content
---

Some content`
	require.NoError(t, os.WriteFile(
		filepath.Join(invalidSkillDir, config.SkillFileName),
		[]byte(invalidContent),
		0o644,
	))

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)
	skills := svc.ListSkills()
	assert.Len(t, skills, 1)
	assert.NotNil(t, svc.GetSkill("valid"))
	assert.Nil(t, svc.GetSkill("invalid"))
}

func TestListSkills(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	skillsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.SkillsDirName)
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))

	skillNames := []string{"alpha", "beta", "gamma"}
	for _, name := range skillNames {
		skillDir := filepath.Join(skillsDir, name)
		require.NoError(t, os.MkdirAll(skillDir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(skillDir, config.SkillFileName),
			[]byte("# "+name),
			0o644,
		))
	}

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)

	skills := svc.ListSkills()
	assert.Len(t, skills, 3)

	names := make(map[string]bool)
	for _, skill := range skills {
		names[skill.Name] = true
	}
	for _, name := range skillNames {
		assert.True(t, names[name], "Expected skill %s to be present", name)
	}
}

func TestListUserInvocableSkills(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	skillsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.SkillsDirName)
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))

	testCases := []struct {
		name          string
		frontmatter   string
		shouldBeShown bool
	}{
		{
			name:          "visible-default",
			frontmatter:   "---\nname: visible-default\ndescription: Default visible skill\n---\n",
			shouldBeShown: true, // default is true
		},
		{
			name:          "visible-explicit",
			frontmatter:   "---\nname: visible-explicit\ndescription: Explicitly visible\nuser-invocable: true\n---\n",
			shouldBeShown: true,
		},
		{
			name:          "hidden",
			frontmatter:   "---\nname: hidden\ndescription: Hidden skill\nuser-invocable: false\n---\n",
			shouldBeShown: false,
		},
		{
			name:          "no-frontmatter",
			frontmatter:   "# No frontmatter\nJust content",
			shouldBeShown: true, // no frontmatter = default = visible
		},
	}

	for _, tc := range testCases {
		skillDir := filepath.Join(skillsDir, tc.name)
		require.NoError(t, os.MkdirAll(skillDir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(skillDir, config.SkillFileName),
			[]byte(tc.frontmatter),
			0o644,
		))
	}

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)

	allSkills := svc.ListSkills()
	assert.Len(t, allSkills, 4)

	visibleSkills := svc.ListUserInvocableSkills()
	assert.Len(t, visibleSkills, 3) // 3 visible, 1 hidden

	visibleNames := make(map[string]bool)
	for _, skill := range visibleSkills {
		visibleNames[skill.Name] = true
	}

	assert.True(t, visibleNames["visible-default"])
	assert.True(t, visibleNames["visible-explicit"])
	assert.True(t, visibleNames["no-frontmatter"])
	assert.False(t, visibleNames["hidden"]) // should not be present

	hidden := svc.GetSkill("hidden")
	require.NotNil(t, hidden)
	assert.False(t, hidden.IsUserInvocable())
}

func TestSkillVisibilityMatrix(t *testing.T) {
	userDisabled := false

	svc := New()
	svc.RegisterSkill(&Skill{Name: "both"})
	svc.RegisterSkill(&Skill{Name: "model-only", UserInvocable: &userDisabled})
	svc.RegisterSkill(&Skill{Name: "user-only", DisableModelInvocation: true})
	svc.RegisterSkill(&Skill{
		Name:                   "neither",
		UserInvocable:          &userDisabled,
		DisableModelInvocation: true,
	})

	userSkills := svc.ListUserInvocableSkills()
	modelSkills := svc.ListModelInvocableSkills()

	assert.Equal(t, []string{"both", "user-only"}, skillNames(userSkills))
	assert.Equal(t, []string{"both", "model-only"}, skillNames(modelSkills))
	assert.True(t, svc.GetSkill("both").IsUserInvocable())
	assert.True(t, svc.GetSkill("both").IsModelInvocable())
}

func TestSkillAnnouncementDescription(t *testing.T) {
	for _, tc := range []struct {
		name        string
		description string
		wantRunes   int
	}{
		{name: "ascii_at_limit", description: strings.Repeat("a", skillDescriptionMaxRunes), wantRunes: 1536},
		{name: "ascii_over_limit", description: strings.Repeat("a", skillDescriptionMaxRunes+1), wantRunes: 1536},
		{name: "multibyte_at_limit", description: strings.Repeat("界", skillDescriptionMaxRunes), wantRunes: 1536},
		{name: "multibyte_over_limit", description: strings.Repeat("界", skillDescriptionMaxRunes+1), wantRunes: 1536},
	} {
		t.Run(tc.name, func(t *testing.T) {
			description := (&Skill{Description: tc.description}).AnnouncementDescription()

			assert.Equal(t, tc.wantRunes, utf8.RuneCountInString(description))
			assert.True(t, utf8.ValidString(description))
			assert.NotContains(t, description, "…")
		})
	}
}

func TestGetSkill(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	skillsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.SkillsDirName)
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))

	skillDir := filepath.Join(skillsDir, "existent")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, config.SkillFileName),
		[]byte("Content"),
		0o644,
	))

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)

	skill := svc.GetSkill("existent")
	assert.NotNil(t, skill)
	assert.Equal(t, "existent", skill.Name)

	assert.Nil(t, svc.GetSkill("nonexistent"))
}

func TestLoadSubagents(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	agentsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.AgentsDirName)
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))

	codeReviewerContent := `---
name: code-reviewer
description: Reviews code for quality and bugs
tools: ["read", "grep", "bash"]
model: claude-sonnet-4-5-20251022
---

You are an expert code reviewer. Focus on:
- Code quality
- Potential bugs
- Performance issues`
	require.NoError(t, os.WriteFile(
		filepath.Join(agentsDir, "code-reviewer.md"),
		[]byte(codeReviewerContent),
		0o644,
	))

	docWriterContent := `---
name: doc-writer
description: Writes documentation
tools: ["*"]
---

You are a technical writer. Create clear, concise documentation.`
	require.NoError(t, os.WriteFile(
		filepath.Join(agentsDir, "doc-writer.md"),
		[]byte(docWriterContent),
		0o644,
	))

	svc := New()
	err := svc.LoadSubagents(tempDir)
	require.NoError(t, err)
	agents := svc.ListSubagents()
	assert.Len(t, agents, 2)

	reviewer := svc.GetSubagent("code-reviewer")
	require.NotNil(t, reviewer)
	assert.Equal(t, "code-reviewer", reviewer.Name)
	assert.Equal(t, "Reviews code for quality and bugs", reviewer.Description)
	assert.Equal(t, []string{"read", "grep", "bash"}, reviewer.Tools)
	assert.Equal(t, "claude-sonnet-4-5-20251022", reviewer.Model)
	assert.Contains(t, reviewer.Prompt, "expert code reviewer")

	writer := svc.GetSubagent("doc-writer")
	require.NotNil(t, writer)
	assert.Equal(t, "doc-writer", writer.Name)
	assert.Equal(t, "Writes documentation", writer.Description)
	assert.Equal(t, []string{"*"}, writer.Tools)
	assert.Contains(t, writer.Prompt, "technical writer")
}

func TestLoadSubagents_NoFrontmatter(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	agentsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.AgentsDirName)
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))

	content := `# Simple Agent

This agent has no frontmatter.

It uses the filename as its name.`
	require.NoError(t, os.WriteFile(
		filepath.Join(agentsDir, "simple-agent.md"),
		[]byte(content),
		0o644,
	))

	svc := New()
	err := svc.LoadSubagents(tempDir)
	require.NoError(t, err)
	agents := svc.ListSubagents()
	assert.Len(t, agents, 1)

	agent := svc.GetSubagent("simple-agent")
	require.NotNil(t, agent)
	assert.Equal(t, "simple-agent", agent.Name)
	assert.Empty(t, agent.Description)
	assert.Empty(t, agent.Tools)
	assert.Contains(t, agent.Prompt, "no frontmatter")
}

func TestLoadSubagents_NoAgentsDir(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	svc := New()
	err := svc.LoadSubagents(tempDir)
	require.NoError(t, err)
	agents := svc.ListSubagents()
	assert.Empty(t, agents)
}

func TestLoadSubagents_NonMarkdownFiles(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	agentsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.AgentsDirName)
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))

	require.NoError(t, os.WriteFile(
		filepath.Join(agentsDir, "valid.md"),
		[]byte("Valid agent"),
		0o644,
	))

	require.NoError(t, os.WriteFile(
		filepath.Join(agentsDir, "ignored.txt"),
		[]byte("Not an agent"),
		0o644,
	))

	require.NoError(t, os.MkdirAll(filepath.Join(agentsDir, "subdir"), 0o755))

	svc := New()
	err := svc.LoadSubagents(tempDir)
	require.NoError(t, err)
	agents := svc.ListSubagents()
	assert.Len(t, agents, 1)
	assert.NotNil(t, svc.GetSubagent("valid"))
	assert.Nil(t, svc.GetSubagent("ignored"))
}

func TestListSubagents(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	agentsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.AgentsDirName)
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))

	agentNames := []string{"agent-a", "agent-b", "agent-c"}
	for _, name := range agentNames {
		content := `---
name: ` + name + `
---

Content for ` + name
		require.NoError(t, os.WriteFile(
			filepath.Join(agentsDir, name+".md"),
			[]byte(content),
			0o644,
		))
	}

	svc := New()
	err := svc.LoadSubagents(tempDir)
	require.NoError(t, err)

	agents := svc.ListSubagents()
	assert.Len(t, agents, 3)

	names := make(map[string]bool)
	for _, agent := range agents {
		names[agent.Name] = true
	}
	for _, name := range agentNames {
		assert.True(t, names[name], "Expected subagent %s to be present", name)
	}
}

func TestGetSubagent(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	agentsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.AgentsDirName)
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))

	require.NoError(t, os.WriteFile(
		filepath.Join(agentsDir, "existent.md"),
		[]byte("Content"),
		0o644,
	))

	svc := New()
	err := svc.LoadSubagents(tempDir)
	require.NoError(t, err)

	agent := svc.GetSubagent("existent")
	assert.NotNil(t, agent)
	assert.Equal(t, "existent", agent.Name)

	assert.Nil(t, svc.GetSubagent("nonexistent"))
}

func TestLoadAgentsMD(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	rootContent := `# Project Guidelines

Follow these guidelines when working on this project.`
	require.NoError(t, os.WriteFile(
		filepath.Join(tempDir, config.ContextFileName),
		[]byte(rootContent),
		0o644,
	))

	svc := New()
	result, err := svc.LoadAgentsMD(tempDir)
	require.NoError(t, err)
	assert.Contains(t, result, "Project Guidelines")
	assert.Contains(t, result, "Follow these guidelines")
}

func TestLoadAgentsMD_MultipleFiles(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	workDir := t.TempDir()
	claudeDir := filepath.Join(workDir, config.ProjectConfigDir)
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))

	agentsContent := `# Agents Guidelines

Agents level guidelines.`
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, config.AgentsFileName),
		[]byte(agentsContent),
		0o644,
	))

	rootContent := `# Root Guidelines

Root level guidelines.`
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, config.ContextFileName),
		[]byte(rootContent),
		0o644,
	))

	configContent := `# Config Guidelines

Config directory guidelines.`
	require.NoError(t, os.WriteFile(
		filepath.Join(claudeDir, config.ContextFileName),
		[]byte(configContent),
		0o644,
	))

	svc := New()
	result, err := svc.LoadAgentsMD(workDir)
	require.NoError(t, err)
	assert.Equal(t, "[project] "+filepath.Join(workDir, config.AgentsFileName)+"\n\n"+agentsContent, result)
	assert.NotContains(t, result, "Root Guidelines")
	assert.NotContains(t, result, "Config Guidelines")
}

func TestLoadAgentsMD_NoFiles(t *testing.T) {
	tempDir := t.TempDir()
	// Isolate from global CLAUDE.md in ~/.codex
	t.Setenv("HOME", tempDir)

	svc := New()
	result, err := svc.LoadAgentsMD(tempDir)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestLoadAgentsMD_EmptyFiles(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	workDir := t.TempDir()
	claudeDir := filepath.Join(workDir, config.ProjectConfigDir)
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))

	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, config.ContextFileName),
		[]byte("   \n\t\n  "),
		0o644,
	))

	require.NoError(t, os.WriteFile(
		filepath.Join(claudeDir, config.ContextFileName),
		[]byte("Valid content"),
		0o644,
	))

	svc := New()
	result, err := svc.LoadAgentsMD(workDir)
	require.NoError(t, err)
	assert.Equal(
		t,
		"[project] "+filepath.Join(workDir, config.ProjectConfigDir, config.ContextFileName)+"\n\nValid content",
		result,
	)
}

func TestLoadAgentsMD_WhitespaceTrimming(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	workDir := t.TempDir()

	content := `

   
# Content

With surrounding whitespace.   

`
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, config.ContextFileName),
		[]byte(content),
		0o644,
	))

	svc := New()
	result, err := svc.LoadAgentsMD(workDir)
	require.NoError(t, err)
	assert.Equal(
		t,
		"[project] "+filepath.Join(workDir, config.ContextFileName)+"\n\n# Content\n\nWith surrounding whitespace.",
		result,
	)
}

func TestLoadAgentsMD_GlobalChainFirstNonEmptyWins(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	workDir := t.TempDir()

	bodies := []struct {
		path string
		body string
	}{
		{filepath.Join(tmpHome, coagenthome.DirName, config.AgentsFileName), "coagent global body"},
		{filepath.Join(tmpHome, ".claude", config.ContextFileName), "claude global body"},
		{filepath.Join(tmpHome, ".codex", config.AgentsFileName), "codex global body"},
		{filepath.Join(tmpHome, ".config", "opencode", config.AgentsFileName), "opencode global body"},
		{filepath.Join(tmpHome, ".gemini", "GEMINI.md"), "gemini global body"},
	}
	for _, c := range bodies {
		require.NoError(t, os.MkdirAll(filepath.Dir(c.path), 0o755))
		require.NoError(t, os.WriteFile(c.path, []byte(c.body), 0o644))
	}

	svc := New()
	result, err := svc.LoadAgentsMD(workDir)
	require.NoError(t, err)
	assert.Equal(
		t,
		"[global] ~/"+coagenthome.DirName+"/"+config.AgentsFileName+"\n\ncoagent global body",
		result,
	)
	for _, c := range bodies[1:] {
		assert.NotContains(t, result, c.body)
	}
}

func TestLoadAgentsMD_GlobalFallbackToClaude(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	workDir := t.TempDir()

	claudePath := filepath.Join(tmpHome, ".claude", config.ContextFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(claudePath), 0o755))
	require.NoError(t, os.WriteFile(claudePath, []byte("claude global body"), 0o644))

	svc := New()
	result, err := svc.LoadAgentsMD(workDir)
	require.NoError(t, err)
	assert.Equal(t, "[global] ~/.claude/"+config.ContextFileName+"\n\nclaude global body", result)
}

func TestLoadAgentsMD_GlobalBrokenSymlinkSkipped(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	workDir := t.TempDir()

	coagentPath := filepath.Join(tmpHome, coagenthome.DirName, config.AgentsFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(coagentPath), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(tmpHome, "deleted-target.md"), coagentPath))

	claudePath := filepath.Join(tmpHome, ".claude", config.ContextFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(claudePath), 0o755))
	require.NoError(t, os.WriteFile(claudePath, []byte("claude global body"), 0o644))

	svc := New()
	result, err := svc.LoadAgentsMD(workDir)
	require.NoError(t, err)
	assert.Equal(t, "[global] ~/.claude/"+config.ContextFileName+"\n\nclaude global body", result)
}

func TestLoadAgentsMD_ProjectChainFirstNonEmptyWins(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	workDir := t.TempDir()
	claudeDir := filepath.Join(workDir, config.ProjectConfigDir)
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))

	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, config.AgentsFileName),
		[]byte("agents body"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, config.ContextFileName),
		[]byte("claude body"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(claudeDir, config.ContextFileName),
		[]byte("nested claude body"),
		0o644,
	))

	svc := New()
	result, err := svc.LoadAgentsMD(workDir)
	require.NoError(t, err)
	assert.Equal(
		t,
		"[project] "+filepath.Join(workDir, config.AgentsFileName)+"\n\nagents body",
		result,
	)
	assert.NotContains(t, result, "claude body")
	assert.NotContains(t, result, "nested claude body")
}

func TestLoadAgentsMD_SymlinkAliasLoadedOnce(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	workDir := t.TempDir()

	body := "aliased body"
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, config.ContextFileName),
		[]byte(body),
		0o644,
	))
	require.NoError(t, os.Symlink(
		filepath.Join(workDir, config.ContextFileName),
		filepath.Join(workDir, config.AgentsFileName),
	))

	svc := New()
	result, err := svc.LoadAgentsMD(workDir)
	require.NoError(t, err)
	assert.Equal(t, "[project] "+filepath.Join(workDir, config.AgentsFileName)+"\n\n"+body, result)
	assert.Equal(t, 1, strings.Count(result, body))
}

func TestLoadAgentsMD_LocalIsAppendNotReplacement(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	workDir := t.TempDir()

	claudeLocal := config.ContextLocalFileName
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, config.ContextFileName),
		[]byte("project body"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, claudeLocal),
		[]byte("local body"),
		0o644,
	))

	svc := New()
	result, err := svc.LoadAgentsMD(workDir)
	require.NoError(t, err)
	assert.Equal(
		t,
		"[project] "+filepath.Join(workDir, config.ContextFileName)+"\n\nproject body\n\n"+
			"[local] "+filepath.Join(workDir, claudeLocal)+"\n\nlocal body",
		result,
	)
}

func TestLoadAgentsMD_LocalChainAGENTSFirst(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	workDir := t.TempDir()

	agentsLocal := config.AgentsLocalFileName
	claudeLocal := config.ContextLocalFileName
	require.NoError(t, os.WriteFile(filepath.Join(workDir, agentsLocal), []byte("agents local body"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(workDir, claudeLocal), []byte("claude local body"), 0o644))

	svc := New()
	result, err := svc.LoadAgentsMD(workDir)
	require.NoError(t, err)
	assert.Equal(t, "[local] "+filepath.Join(workDir, agentsLocal)+"\n\nagents local body", result)
	assert.NotContains(t, result, "claude local body")
}

func TestLoadAgentsMD_MergeLabelsAndTildeRendering(t *testing.T) {
	t.Run("workDir under HOME renders the tilde prefix", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		workDir := filepath.Join(tmpHome, "project")
		require.NoError(t, os.MkdirAll(workDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(workDir, config.AgentsFileName), []byte("project body"), 0o644))

		svc := New()
		result, err := svc.LoadAgentsMD(workDir)
		require.NoError(t, err)
		assert.Equal(t, "[project] ~/project/"+config.AgentsFileName+"\n\nproject body", result)
	})

	t.Run("workDir outside HOME renders the absolute path", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		workDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(workDir, config.AgentsFileName), []byte("project body"), 0o644))

		svc := New()
		result, err := svc.LoadAgentsMD(workDir)
		require.NoError(t, err)
		assert.Equal(t, "[project] "+filepath.Join(workDir, config.AgentsFileName)+"\n\nproject body", result)
	})
}

func TestLoadAgentsMD_NoArtifacts(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	workDir := t.TempDir()

	svc := New()
	result, err := svc.LoadAgentsMD(workDir)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestLoadAgentsMD_DirectoryCandidateSkippedWithRecordedError(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	workDir := t.TempDir()

	agentsPath := filepath.Join(workDir, config.AgentsFileName)
	require.NoError(t, os.MkdirAll(agentsPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workDir, config.ContextFileName), []byte("claude body"), 0o644))

	svc := New()
	result, err := svc.LoadAgentsMD(workDir)
	require.Error(t, err)
	require.ErrorContains(t, err, agentsPath)
	assert.Equal(t, "[project] "+filepath.Join(workDir, config.ContextFileName)+"\n\nclaude body", result)
}

func TestLoadAgentsMD_EscapingSymlinkRejected(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(project, 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(outside, config.AgentsFileName),
		[]byte("outside context"),
		0o644,
	))
	require.NoError(t, os.Symlink(
		filepath.Join(outside, config.AgentsFileName),
		filepath.Join(project, config.AgentsFileName),
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(project, config.ContextFileName),
		[]byte("project claude body"),
		0o644,
	))

	svc := New()
	result, err := svc.LoadAgentsMD(project)
	require.Error(t, err)
	require.ErrorContains(t, err, safefile.ErrOutsideProject.Error())
	assert.NotContains(t, result, "outside context")
	assert.Equal(
		t,
		"[project] "+filepath.Join(project, config.ContextFileName)+"\n\nproject claude body",
		result,
	)
}

func TestLoadAgentsMD_EscapingSymlinkOnlyCandidate(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(project, 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(outside, config.AgentsFileName),
		[]byte("outside context"),
		0o644,
	))
	require.NoError(t, os.Symlink(
		filepath.Join(outside, config.AgentsFileName),
		filepath.Join(project, config.AgentsFileName),
	))

	svc := New()
	result, err := svc.LoadAgentsMD(project)
	require.Error(t, err)
	require.ErrorContains(t, err, safefile.ErrOutsideProject.Error())
	assert.Empty(t, result)
}

func TestLoadSkills_MissingWorkDirKeepsGlobalSources(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	globalSkillDir := filepath.Join(tmpHome, config.ProjectCoagentDir, config.SkillsDirName, "demo")
	require.NoError(t, os.MkdirAll(globalSkillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(globalSkillDir, config.SkillFileName),
		[]byte("global skill body"),
		0o644,
	))

	svc := New()
	err := svc.LoadSkills(filepath.Join(tmpHome, "missing-workdir"))
	require.Error(t, err)
	require.NotNil(t, svc.GetSkill("demo"))
}

func TestLoadSubagents_MissingWorkDirKeepsGlobalSources(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	require.NoError(t, os.MkdirAll(
		filepath.Join(tmpHome, config.ProjectCoagentDir, config.AgentsDirName),
		0o755,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpHome, config.ProjectCoagentDir, config.AgentsDirName, "demo.md"),
		[]byte("global subagent body"),
		0o644,
	))

	svc := New()
	err := svc.LoadSubagents(filepath.Join(tmpHome, "missing-workdir"))
	require.Error(t, err)
	require.NotNil(t, svc.GetSubagent("demo"))
}

func TestLoadSkills_MarketplaceEscapingSymlinkRejected(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	skillsDir := filepath.Join(repo, "plugins", "plug", config.SkillsDirName)
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(outside, "secrets.txt"),
		[]byte("outside secret"),
		0o644,
	))
	require.NoError(t, os.Symlink(
		filepath.Join(outside, "secrets.txt"),
		filepath.Join(skillsDir, "escape.md"),
	))
	require.NoError(t, os.WriteFile(filepath.Join(skillsDir, "ok.md"), []byte("fine skill"), 0o644))

	svc := New().(*svc)
	svc.marketplaceSkillPaths = []sourceInfo{{path: skillsDir, pluginName: "plug", root: repo}}
	require.NoError(t, svc.LoadSkills(t.TempDir()))

	assert.Nil(t, svc.GetSkill("plug:escape"))
	require.NotNil(t, svc.GetSkill("plug:ok"))
}

// The containment root is the repository clone, not the cache base: a symlink
// crossing to a sibling repo under the same cache base must be denied.
func TestLoadSkills_MarketplaceSiblingRepoSymlinkRejected(t *testing.T) {
	base := t.TempDir()
	cacheBase := filepath.Join(base, "cache")
	repoA := filepath.Join(cacheBase, "owner", "repo-a")
	repoB := filepath.Join(cacheBase, "owner", "repo-b")
	skillsDir := filepath.Join(repoA, "plugins", "plug", config.SkillsDirName)
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))
	require.NoError(t, os.MkdirAll(repoB, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoB, "secret.md"), []byte("sibling secret"), 0o644))
	require.NoError(t, os.Symlink(
		filepath.Join(repoB, "secret.md"),
		filepath.Join(skillsDir, "sneak.md"),
	))

	svc := New().(*svc)
	svc.marketplaceSkillPaths = []sourceInfo{{path: skillsDir, pluginName: "plug", root: repoA}}
	require.NoError(t, svc.LoadSkills(t.TempDir()))

	assert.Nil(t, svc.GetSkill("plug:sneak"))
}

func TestIntegration_LoadAll(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	skillsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.SkillsDirName)
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))
	skillDir := filepath.Join(skillsDir, "my-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, config.SkillFileName),
		[]byte("Skill content"),
		0o644,
	))

	agentsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.AgentsDirName)
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(agentsDir, "my-agent.md"),
		[]byte("Agent content"),
		0o644,
	))

	require.NoError(t, os.WriteFile(
		filepath.Join(tempDir, config.ContextFileName),
		[]byte("CLAUDE.md content"),
		0o644,
	))

	svc := New()

	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)
	skills := svc.ListSkills()
	assert.Len(t, skills, 1)

	err = svc.LoadSubagents(tempDir)
	require.NoError(t, err)
	agents := svc.ListSubagents()
	assert.Len(t, agents, 1)

	claudeContent, err := svc.LoadAgentsMD(tempDir)
	require.NoError(t, err)
	assert.NotEmpty(t, claudeContent)

	assert.NotNil(t, svc.GetSkill("my-skill"))
	assert.NotNil(t, svc.GetSubagent("my-agent"))
}

func TestLoadSkills_Overwrite(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	skillsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.SkillsDirName)
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))

	skillDir1 := filepath.Join(skillsDir, "skill-1")
	require.NoError(t, os.MkdirAll(skillDir1, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir1, config.SkillFileName),
		[]byte("First content"),
		0o644,
	))

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)
	assert.NotNil(t, svc.GetSkill("skill-1"))

	require.NoError(t, os.RemoveAll(skillDir1))
	skillDir2 := filepath.Join(skillsDir, "skill-2")
	require.NoError(t, os.MkdirAll(skillDir2, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir2, config.SkillFileName),
		[]byte("Second content"),
		0o644,
	))

	err = svc.LoadSkills(tempDir)
	require.NoError(t, err)

	assert.Nil(t, svc.GetSkill("skill-1"))
	assert.NotNil(t, svc.GetSkill("skill-2"))
}

func TestSkillPathField(t *testing.T) {
	tempDir := t.TempDir()
	skillsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.SkillsDirName)
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))

	skillDir := filepath.Join(skillsDir, "test")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	skillPath := filepath.Join(skillDir, config.SkillFileName)
	require.NoError(t, os.WriteFile(skillPath, []byte("Content"), 0o644))

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)

	skill := svc.GetSkill("test")
	require.NotNil(t, skill)
	assert.Equal(t, skillPath, skill.Path)
}

func TestSubagentPathField(t *testing.T) {
	tempDir := t.TempDir()
	agentsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.AgentsDirName)
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))

	agentPath := filepath.Join(agentsDir, "test.md")
	require.NoError(t, os.WriteFile(agentPath, []byte("Content"), 0o644))

	svc := New()
	err := svc.LoadSubagents(tempDir)
	require.NoError(t, err)

	agent := svc.GetSubagent("test")
	require.NotNil(t, agent)
	assert.Equal(t, agentPath, agent.Path)
}

func TestLoadSkills_FromClaudeCommands(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	commandsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.CommandsDirName)
	require.NoError(t, os.MkdirAll(commandsDir, 0o755))

	require.NoError(t, os.WriteFile(
		filepath.Join(commandsDir, "my-cmd.md"),
		[]byte("Run my custom command"),
		0o644,
	))

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)
	skills := svc.ListSkills()
	assert.Len(t, skills, 1)

	cmd := svc.GetSkill("my-cmd")
	require.NotNil(t, cmd)
	assert.Equal(t, "my-cmd", cmd.Name)
	assert.Contains(t, cmd.Content, "Run my custom command")
}

func TestLoadSkills_FromAgentsSkills(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	agentsSkillsDir := filepath.Join(tempDir, config.AgentsConfigDir, config.SkillsDirName, "my-skill")
	require.NoError(t, os.MkdirAll(agentsSkillsDir, 0o755))

	require.NoError(t, os.WriteFile(
		filepath.Join(agentsSkillsDir, config.SkillFileName),
		[]byte("Skill from .agents"),
		0o644,
	))

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)
	skills := svc.ListSkills()
	assert.Len(t, skills, 1)

	skill := svc.GetSkill("my-skill")
	require.NotNil(t, skill)
	assert.Equal(t, "my-skill", skill.Name)
	assert.Contains(t, skill.Content, "Skill from .agents")
}

// TestLoadSkills_ClaudeSkillsOverridesCommands verifies .claude/skills/ wins over .claude/commands/.
func TestLoadSkills_ClaudeSkillsOverridesCommands(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	// Create skill in .claude/commands/
	commandsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.CommandsDirName)
	require.NoError(t, os.MkdirAll(commandsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(commandsDir, "clash.md"),
		[]byte("from commands"),
		0o644,
	))

	// Create skill in .claude/skills/ (higher priority)
	skillsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.SkillsDirName, "clash")
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillsDir, config.SkillFileName),
		[]byte("from skills"),
		0o644,
	))

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)

	skill := svc.GetSkill("clash")
	require.NotNil(t, skill)
	assert.Contains(t, skill.Content, "from skills")
}

// TestLoadSkills_CommandsOverridesAgentsSkills verifies .claude/commands/ wins over .agents/skills/.
func TestLoadSkills_CommandsOverridesAgentsSkills(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	// Create skill in .agents/skills/
	agentsSkillsDir := filepath.Join(tempDir, config.AgentsConfigDir, config.SkillsDirName, "clash")
	require.NoError(t, os.MkdirAll(agentsSkillsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(agentsSkillsDir, config.SkillFileName),
		[]byte("from agents"),
		0o644,
	))

	// Create skill in .claude/commands/ (higher priority)
	commandsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.CommandsDirName)
	require.NoError(t, os.MkdirAll(commandsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(commandsDir, "clash.md"),
		[]byte("from commands"),
		0o644,
	))

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)

	skill := svc.GetSkill("clash")
	require.NotNil(t, skill)
	assert.Contains(t, skill.Content, "from commands")
}

// TestLoadSkills_CoagentSkillsOverridesAgentsSkills verifies .coagent/skills/ wins over .agents/skills/.
func TestLoadSkills_CoagentSkillsOverridesAgentsSkills(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	// Create skill in .agents/skills/
	agentsSkillsDir := filepath.Join(tempDir, config.AgentsConfigDir, config.SkillsDirName, "clash")
	require.NoError(t, os.MkdirAll(agentsSkillsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(agentsSkillsDir, config.SkillFileName),
		[]byte("from agents"),
		0o644,
	))

	// Create skill in .coagent/skills/ (higher priority)
	coagentSkillsDir := filepath.Join(tempDir, config.ProjectCoagentDir, config.SkillsDirName, "clash")
	require.NoError(t, os.MkdirAll(coagentSkillsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(coagentSkillsDir, config.SkillFileName),
		[]byte("from coagent"),
		0o644,
	))

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)

	skill := svc.GetSkill("clash")
	require.NotNil(t, skill)
	assert.Contains(t, skill.Content, "from coagent")
}

// TestLoadSkills_SymlinkedSkillDir verifies a symlink to a skill directory
// (e.g. ~/.coagent/skills/foo -> /path/to/foo) is followed and loaded.
func TestLoadSkills_SymlinkedSkillDir(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	realSkillDir := filepath.Join(tempDir, "real-skill-location", "auto-care")
	require.NoError(t, os.MkdirAll(realSkillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(realSkillDir, config.SkillFileName),
		[]byte("Skill via symlink"),
		0o644,
	))

	globalSkillsDir := filepath.Join(tempDir, coagenthome.DirName, config.SkillsDirName)
	require.NoError(t, os.MkdirAll(globalSkillsDir, 0o755))
	require.NoError(t, os.Symlink(realSkillDir, filepath.Join(globalSkillsDir, "auto-care")))

	svc := New()
	err := svc.LoadSkills(tempDir)
	require.NoError(t, err)

	skill := svc.GetSkill("auto-care")
	require.NotNil(t, skill)
	assert.Contains(t, skill.Content, "Skill via symlink")
}

func skillNames(skills []*Skill) []string {
	names := make([]string, len(skills))
	for i, skill := range skills {
		names[i] = skill.Name
	}

	return names
}
