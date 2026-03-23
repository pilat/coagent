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

func TestContextFilePaths(t *testing.T) {
	t.Run("returns correct paths in order when HOME is set", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		cwd := "/home/user/project"

		paths := contextFilePaths(cwd)
		require.Len(t, paths, 5)

		assert.Equal(
			t,
			filepath.Join(tmpHome, coagenthome.DirName, config.AgentsFileName),
			paths[0],
			"global AGENTS",
		)
		assert.Equal(t, filepath.Join(cwd, config.AgentsFileName), paths[1], "project AGENTS")
		assert.Equal(t, filepath.Join(cwd, config.ContextFileName), paths[2], "project root CLAUDE")
		assert.Equal(
			t,
			filepath.Join(cwd, config.ProjectConfigDir, config.ContextFileName),
			paths[3],
			"project config dir CLAUDE",
		)
		assert.Equal(t, filepath.Join(cwd, "CLAUDE.local.md"), paths[4], "local")
	})

	t.Run("returns 4 paths when HOME is not set", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")
		cwd := "/home/user/project"

		paths := contextFilePaths(cwd)
		require.Len(t, paths, 4)

		assert.Equal(t, filepath.Join(cwd, config.AgentsFileName), paths[0], "project AGENTS")
	})

	t.Run("uses correct local file naming", func(t *testing.T) {
		cwd := "/tmp/test"
		paths := contextFilePaths(cwd)

		lastPath := paths[len(paths)-1]
		assert.True(
			t,
			strings.HasSuffix(lastPath, "CLAUDE.local.md"),
			"local path = %q, should end with CLAUDE.local.md",
			lastPath,
		)
	})

	t.Run("handles different working directories", func(t *testing.T) {
		cwds := []string{
			"/home/user/project",
			"/very/long/path/to/the/project/directory",
			"/tmp/project-with-dashes",
		}

		for _, cwd := range cwds {
			paths := contextFilePaths(cwd)
			require.GreaterOrEqual(t, len(paths), 3, "contextFilePaths(%q)", cwd)

			for _, p := range paths {
				if strings.Contains(p, coagenthome.DirName) {
					continue
				}
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
