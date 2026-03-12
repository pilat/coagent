//go:build darwin

package bashsandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDarwinRunner_CommandUsesParameters(t *testing.T) {
	roots := []string{"/tmp/path with spaces", "/tmp/-cache=value"}
	runner := &darwinRunner{
		executable: seatbeltExecutable,
		profile:    seatbeltProfile(len(roots)),
		parameters: seatbeltParameters(roots),
	}

	commandArgs := []string{"hostile ;$()", "line\nbreak", "-leading=equals"}
	cmd, err := runner.Command(context.Background(), "printf '%s' hello", "/tmp", commandArgs...)
	require.NoError(t, err)

	assert.Equal(t, seatbeltExecutable, cmd.Path)
	assert.Equal(t, "/tmp", cmd.Dir)
	assert.Equal(t, []string{
		seatbeltExecutable,
		"-D", "WRITABLE_0=/tmp/path with spaces",
		"-D", "WRITABLE_1=/tmp/-cache=value",
		"-p", runner.profile,
		"bash", "-c", "printf '%s' hello",
		"hostile ;$()", "line\nbreak", "-leading=equals",
	}, cmd.Args)
	assert.NotContains(t, runner.profile, roots[0])
	assert.NotContains(t, runner.profile, roots[1])
	assert.Contains(t, runner.profile, `(param "WRITABLE_0")`)
	assert.Contains(t, runner.profile, `(param "WRITABLE_1")`)
}

func TestDarwinRunner_SeatbeltPolicy(t *testing.T) {
	if _, err := os.Stat(seatbeltExecutable); err != nil {
		t.Skipf("Seatbelt executable unavailable: %v", err)
	}

	root, err := os.MkdirTemp(".", ".coagent-bashsandbox-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.Abs(root)
	require.NoError(t, err)

	workspace := filepath.Join(root, "workspace")
	sibling := filepath.Join(root, "sibling")
	require.NoError(t, os.Mkdir(workspace, 0o755))
	require.NoError(t, os.Mkdir(sibling, 0o755))

	outsideFile := filepath.Join(sibling, "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("outside"), 0o600))

	runner, err := New(Config{Enabled: true, WorkDir: workspace}, nil)
	require.NoError(t, err)

	run := func(t *testing.T, command, workDir string) (string, error) {
		t.Helper()
		cmd, err := runner.Command(context.Background(), command, workDir)
		require.NoError(t, err)
		output, err := cmd.CombinedOutput()
		return string(output), err
	}

	t.Run("reads outside workspace", func(t *testing.T) {
		output, err := run(t, "cat "+shellQuote(outsideFile), workspace)
		require.NoError(t, err, output)
		assert.Equal(t, "outside", output)
	})

	t.Run("writes workspace temp and child process", func(t *testing.T) {
		workspaceFile := filepath.Join(workspace, "workspace.txt")
		tempFile := filepath.Join(os.TempDir(), "coagent-bashsandbox-seatbelt-"+filepath.Base(root)+".txt")
		tmpAliasFile := filepath.Join("/tmp", "coagent-bashsandbox-seatbelt-alias-"+filepath.Base(root)+".txt")
		t.Cleanup(func() { _ = os.Remove(tempFile) })
		t.Cleanup(func() { _ = os.Remove(tmpAliasFile) })

		command := "printf workspace >" + shellQuote(workspaceFile) +
			" && bash -c " + shellQuote("printf child >"+shellQuote(tempFile)) +
			" && printf alias >" + shellQuote(tmpAliasFile)
		output, err := run(t, command, workspace)
		require.NoError(t, err, output)
		assert.FileExists(t, workspaceFile)
		assert.FileExists(t, tempFile)
		assert.FileExists(t, tmpAliasFile)
	})

	t.Run("denies sibling writes", func(t *testing.T) {
		denied := filepath.Join(sibling, "denied.txt")
		output, err := run(t, "printf denied >"+shellQuote(denied), workspace)
		require.Error(t, err, output)
		assert.NoFileExists(t, denied)
	})

	t.Run("workdir does not grant writes", func(t *testing.T) {
		output, err := run(t, "printf denied >relative.txt", sibling)
		require.Error(t, err, output)
		assert.NoFileExists(t, filepath.Join(sibling, "relative.txt"))
	})

	t.Run("symlink does not grant writes", func(t *testing.T) {
		link := filepath.Join(workspace, "outside-link")
		require.NoError(t, os.Symlink(outsideFile, link))

		output, err := run(t, "printf mutated >"+shellQuote(link), workspace)
		require.Error(t, err, output)

		content, err := os.ReadFile(outsideFile)
		require.NoError(t, err)
		assert.Equal(t, "outside", string(content))
	})

	t.Run("inherits environment and host services", func(t *testing.T) {
		t.Setenv("COAGENT_SANDBOX_TEST", "inherited")
		commands := []string{
			`test "$COAGENT_SANDBOX_TEST" = inherited`,
			"printf device >/dev/null",
			"test -r /etc/ssl/cert.pem",
			"git --version",
			"go test . -run '^$'",
		}

		packageDir, err := os.Getwd()
		require.NoError(t, err)

		output, err := run(t, strings.Join(commands, " && "), packageDir)
		require.NoError(t, err, output)
	})
}

func TestDarwinRunner_ProbeConfirmsEnforcement(t *testing.T) {
	if _, err := os.Stat(seatbeltExecutable); err != nil {
		t.Skipf("Seatbelt executable unavailable: %v", err)
	}

	require.NoError(t, probeEnforcement(newEnabledRunner))
}

func TestDarwinRunner_CommandExitStatusIsPreserved(t *testing.T) {
	runner, err := New(Config{Enabled: true, WorkDir: t.TempDir()}, nil)
	require.NoError(t, err)

	cmd, err := runner.Command(context.Background(), "exit 17", t.TempDir())
	require.NoError(t, err)

	err = cmd.Run()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 17, exitErr.ExitCode())
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
