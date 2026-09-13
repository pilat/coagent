package loader

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
)

func TestGlobalDir(t *testing.T) {
	t.Run("returns the coagent home under HOME", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)

		got := globalDir()
		expected := filepath.Join(tmpHome, coagenthome.DirName)
		assert.Equal(t, expected, got)
	})

	t.Run("returns empty string when HOME is not set", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")

		assert.Empty(t, globalDir())
	})
}

func TestProjectDir(t *testing.T) {
	tests := []struct {
		name     string
		cwd      string
		expected string
	}{
		{"absolute path", "/home/user/project", filepath.Join("/home/user/project", config.ProjectConfigDir)},
		{"relative path", "./my-project", filepath.Join("./my-project", config.ProjectConfigDir)},
		{"path with spaces", "/home/user/my project", filepath.Join("/home/user/my project", config.ProjectConfigDir)},
		{"empty path", "", config.ProjectConfigDir},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, projectDir(tt.cwd))
		})
	}
}

func TestGlobalSkillsDir(t *testing.T) {
	t.Run("returns path when HOME is set", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)

		expected := filepath.Join(tmpHome, coagenthome.DirName, config.SkillsDirName)
		assert.Equal(t, expected, globalSkillsDir())
	})

	t.Run("returns empty string when HOME is not set", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")

		assert.Empty(t, globalSkillsDir())
	})
}

func TestProjectSkillsDir(t *testing.T) {
	tests := []struct {
		name     string
		cwd      string
		expected string
	}{
		{
			"standard project path",
			"/home/user/project",
			filepath.Join("/home/user/project", config.ProjectConfigDir, config.SkillsDirName),
		},
		{
			"nested project path",
			"/home/user/projects/myapp",
			filepath.Join("/home/user/projects/myapp", config.ProjectConfigDir, config.SkillsDirName),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, projectSkillsDir(tt.cwd))
		})
	}
}

func TestGlobalAgentsDir(t *testing.T) {
	t.Run("returns path when HOME is set", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)

		expected := filepath.Join(tmpHome, coagenthome.DirName, config.AgentsDirName)
		assert.Equal(t, expected, globalAgentsDir())
	})

	t.Run("returns empty string when HOME is not set", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")

		assert.Empty(t, globalAgentsDir())
	})
}

func TestProjectAgentsDir(t *testing.T) {
	tests := []struct {
		name     string
		cwd      string
		expected string
	}{
		{
			"standard project path",
			"/home/user/project",
			filepath.Join("/home/user/project", config.ProjectConfigDir, config.AgentsDirName),
		},
		{
			"project with spaces in path",
			"/home/user/my project",
			filepath.Join("/home/user/my project", config.ProjectConfigDir, config.AgentsDirName),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, projectAgentsDir(tt.cwd))
		})
	}
}

func TestProjectCommandsDir(t *testing.T) {
	tests := []struct {
		name     string
		cwd      string
		expected string
	}{
		{
			"standard project path",
			"/home/user/project",
			filepath.Join("/home/user/project", config.ProjectConfigDir, config.CommandsDirName),
		},
		{
			"nested project path",
			"/home/user/projects/myapp",
			filepath.Join("/home/user/projects/myapp", config.ProjectConfigDir, config.CommandsDirName),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, projectCommandsDir(tt.cwd))
		})
	}
}

func TestProjectAgentsSkillsDir(t *testing.T) {
	tests := []struct {
		name     string
		cwd      string
		expected string
	}{
		{
			"standard project path",
			"/home/user/project",
			filepath.Join("/home/user/project", config.AgentsConfigDir, config.SkillsDirName),
		},
		{
			"nested project path",
			"/home/user/projects/myapp",
			filepath.Join("/home/user/projects/myapp", config.AgentsConfigDir, config.SkillsDirName),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, projectAgentsSkillsDir(tt.cwd))
		})
	}
}

func TestContextCandidateLists(t *testing.T) {
	t.Run("global candidates are the five home-relative paths in order", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)

		candidates := globalContextCandidates()
		require.Len(t, candidates, 5)

		assert.Equal(
			t,
			filepath.Join(tmpHome, coagenthome.DirName, config.AgentsFileName),
			candidates[0],
			"coagent global AGENTS",
		)
		assert.Equal(
			t,
			filepath.Join(tmpHome, ".claude", config.ContextFileName),
			candidates[1],
			"claude global",
		)
		assert.Equal(
			t,
			filepath.Join(tmpHome, ".codex", config.AgentsFileName),
			candidates[2],
			"codex global AGENTS",
		)
		assert.Equal(
			t,
			filepath.Join(tmpHome, ".config", "opencode", config.AgentsFileName),
			candidates[3],
			"opencode global AGENTS",
		)
		assert.Equal(t, filepath.Join(tmpHome, ".gemini", "GEMINI.md"), candidates[4], "gemini global")
	})

	t.Run("global candidates are empty when HOME is not set", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")

		assert.Empty(t, globalContextCandidates())
	})

	t.Run("project candidates are workDir-relative in order", func(t *testing.T) {
		cwd := "/home/user/project"

		candidates := projectContextCandidates(cwd)
		require.Len(t, candidates, 3)

		assert.Equal(t, filepath.Join(cwd, config.AgentsFileName), candidates[0], "project AGENTS")
		assert.Equal(t, filepath.Join(cwd, config.ContextFileName), candidates[1], "project root CLAUDE")
		assert.Equal(
			t,
			filepath.Join(cwd, config.ProjectConfigDir, config.ContextFileName),
			candidates[2],
			"project config dir CLAUDE",
		)
	})

	t.Run("local candidates are workDir-relative in order", func(t *testing.T) {
		cwd := "/home/user/project"

		candidates := localContextCandidates(cwd)
		require.Len(t, candidates, 2)

		assert.Equal(t, filepath.Join(cwd, config.AgentsLocalFileName), candidates[0], "local AGENTS")
		assert.Equal(t, filepath.Join(cwd, config.ContextLocalFileName), candidates[1], "local CLAUDE")
	})

	t.Run("project and local candidates track different working directories", func(t *testing.T) {
		cwds := []string{
			"/home/user/project",
			"/very/long/path/to/the/project/directory",
			"/tmp/project-with-dashes",
		}

		for _, cwd := range cwds {
			for _, p := range append(projectContextCandidates(cwd), localContextCandidates(cwd)...) {
				assert.Contains(t, p, cwd, "path should contain cwd")
			}
		}
	})
}

func TestDirectoryIntegration(t *testing.T) {
	t.Run("skills and agents directories are subdirectories of config dirs", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		cwd := "/tmp/project"

		gd := globalDir()
		assert.True(t, strings.HasPrefix(globalSkillsDir(), gd), "globalSkillsDir should be under globalDir")
		assert.True(t, strings.HasPrefix(globalAgentsDir(), gd), "globalAgentsDir should be under globalDir")

		pd := projectDir(cwd)
		assert.True(t, strings.HasPrefix(projectSkillsDir(cwd), pd), "projectSkillsDir should be under projectDir")
		assert.True(t, strings.HasPrefix(projectAgentsDir(cwd), pd), "projectAgentsDir should be under projectDir")
	})
}
