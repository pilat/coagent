package loader

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/safefile"
)

func TestShieldedLoaderRejectsProjectInstructionSymlinksOutsideRoot(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(project, 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, config.AgentsFileName), []byte("outside context"), 0o600))
	require.NoError(t, os.Symlink(
		filepath.Join(outside, config.AgentsFileName), filepath.Join(project, config.AgentsFileName),
	))

	access, err := safefile.New(project, safefile.ProjectConfined)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, access.Close()) })
	service := New()
	service.SetProjectAccess(access)

	contextText, err := service.LoadAgentsMD(project)
	require.Error(t, err)
	assert.NotContains(t, contextText, "outside context")
	assert.ErrorContains(t, err, safefile.ShieldDeniedMessage)
}

func TestShieldedLoaderSkipsOutsideProjectSkillAndSubagentSymlinks(t *testing.T) {
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

	access, err := safefile.New(project, safefile.ProjectConfined)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, access.Close()) })
	service := New()
	service.SetProjectAccess(access)

	require.NoError(t, service.LoadSkills(project))
	require.NoError(t, service.LoadSubagents(project))
	assert.Nil(t, service.GetSkill("escape"))
	assert.Nil(t, service.GetSubagent("escape"))
}
