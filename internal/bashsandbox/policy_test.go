package bashsandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShieldPolicyExecutableLookupDoesNotGrantPATHAuthority(t *testing.T) {
	project := t.TempDir()
	external := t.TempDir()
	externalTool := filepath.Join(external, "external-tool")
	require.NoError(t, os.WriteFile(externalTool, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	projectTool := filepath.Join(project, "project-tool")
	require.NoError(t, os.WriteFile(projectTool, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", external+string(os.PathListSeparator)+project)

	policy, err := buildProcessPolicy(Config{
		WorkDir: project, SessionKey: "session:1", ReadScope: ProjectConfined,
	}, []string{project})
	require.NoError(t, err)

	_, err = policy.resolveExecutable("external-tool", project)
	require.ErrorContains(t, err, "outside the shielded project")
	resolved, err := policy.resolveExecutable("project-tool", project)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(policy.projectRoot, "project-tool"), resolved)
	resolved, err = policy.resolveExecutable("./project-tool", project)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(policy.projectRoot, "project-tool"), resolved)
}

func TestProcessPolicyKeyIncludesSessionAndReadScope(t *testing.T) {
	project := t.TempDir()
	down, err := buildProcessPolicy(Config{
		WorkDir: project, SessionKey: "session:1", ReadScope: HostReadable,
	}, []string{project})
	require.NoError(t, err)
	up, err := buildProcessPolicy(Config{
		WorkDir: project, SessionKey: "session:1", ReadScope: ProjectConfined,
	}, []string{project})
	require.NoError(t, err)
	other, err := buildProcessPolicy(Config{
		WorkDir: project, SessionKey: "session:2", ReadScope: ProjectConfined,
	}, []string{project})
	require.NoError(t, err)

	assert.NotEqual(t, down.key(), up.key())
	assert.NotEqual(t, up.key(), other.key())
}
