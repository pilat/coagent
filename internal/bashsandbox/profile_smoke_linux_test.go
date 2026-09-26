//go:build linux

package bashsandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolchainSmokeFixture lays out one install directory the shipped mise profile
// grants and one sibling directory no profile grants. Both are addressed by
// absolute path: the sandbox clears the environment, so PATH proves nothing.
func toolchainSmokeFixture(t *testing.T) (string, string, string) {
	t.Helper()

	home := testSandboxHome(t)

	granted := filepath.Join(testDir(t,
		filepath.Join(home, ".local", "share", "mise", "installs", "faketool", "1.0.0", "bin")), "faketool")
	require.NoError(t, os.WriteFile(granted, []byte("#!/bin/sh\necho faketool-ok:$PWD\n"), 0o755))

	ungranted := filepath.Join(testDir(t,
		filepath.Join(home, ".local", "share", "unrelated", "bin")), "othertool")
	require.NoError(t, os.WriteFile(ungranted, []byte("#!/bin/sh\necho should-not-run\n"), 0o755))

	return home, granted, ungranted
}

// TestProfileSmoke_ToolchainRunsThroughItsGrant proves a shipped profile makes
// an activated toolchain usable, not merely readable: the granted install
// directory is reachable and its binary runs from the project, while an
// ungranted sibling directory stays invisible.
func TestProfileSmoke_ToolchainRunsThroughItsGrant(t *testing.T) {
	home, granted, ungranted := toolchainSmokeFixture(t)
	project := testDir(t, filepath.Join(home, "project"))

	runner := testRunnerFromPolicy(t, testCompiledPolicy(t, project, false), project, "profile-smoke")

	output, err := runSandboxCommand(t, runner, granted, project)
	require.NoError(t, err, "the granted toolchain must run: %s", output)
	assert.Contains(t, output, "faketool-ok:")
	assert.Contains(t, output, project, "and it runs with the project as its working directory")

	output, err = runSandboxCommand(t, runner, ungranted, project)
	require.Error(t, err, "a directory outside every profile is not reachable")
	assert.NotContains(t, output, "should-not-run")
}

// TestProfileSmoke_ShieldsRemoveTheToolchainProfile proves the same fixture
// fails closed under a shield: no profile, no toolchain.
func TestProfileSmoke_ShieldsRemoveTheToolchainProfile(t *testing.T) {
	home, granted, ungranted := toolchainSmokeFixture(t)
	project := testDir(t, filepath.Join(home, "project"))

	runner := testRunnerFromPolicy(t, testCompiledPolicy(t, project, true), project, "profile-smoke-shielded")

	output, err := runSandboxCommand(t, runner, granted, project)
	require.Error(t, err, "a raised shield keeps no toolchain profile")
	assert.NotContains(t, output, "faketool-ok:")

	output, err = runSandboxCommand(t, runner, ungranted, project)
	require.Error(t, err)
	assert.NotContains(t, output, "should-not-run")
}
