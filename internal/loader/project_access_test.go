//go:build linux

package loader

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestLoaderSkipsOutsideProjectSkillAndSubagentSymlinks(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(filepath.Join(project, config.ProjectConfigDir, config.SkillsDirName), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(project, config.ProjectConfigDir, config.AgentsDirName), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "skill"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(outside, "skill", config.SkillFileName), []byte("outside skill"), 0o600,
	))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "agent.md"), []byte("outside agent"), 0o600))
	require.NoError(t, os.Symlink(
		filepath.Join(outside, "skill"),
		filepath.Join(project, config.ProjectConfigDir, config.SkillsDirName, "escape"),
	))
	require.NoError(t, os.Symlink(
		filepath.Join(outside, "agent.md"),
		filepath.Join(project, config.ProjectConfigDir, config.AgentsDirName, "escape.md"),
	))

	service := New()

	require.NoError(t, service.LoadSkills(project))
	require.NoError(t, service.LoadSubagents(project))
	assert.Nil(t, service.GetSkill("escape"))
	assert.Nil(t, service.GetSubagent("escape"))
}
