//go:build linux

package bashsandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/procexec"
)

func TestBubblewrapIntegration(t *testing.T) {
	if _, err := exec.LookPath(bubblewrapExecutable); err != nil {
		t.Skip("bwrap is not installed")
	}

	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	denied := filepath.Join(base, "denied")
	require.NoError(t, os.Mkdir(allowed, 0o755))
	require.NoError(t, os.Mkdir(denied, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(denied, "readable"), []byte("host data"), 0o644))

	runner, err := newEnabledRunner(processPolicy{
		readScope: HostReadable, workDir: allowed, projectRoot: allowed,
		writableRoots: []string{allowed}, sessionKey: "test",
	})
	require.NoError(t, err)

	devShmName := "coagent-bwrap-" + filepath.Base(base)
	t.Cleanup(func() {
		if _, err := os.Stat(filepath.Join("/dev/shm", devShmName)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("sandbox exposed host /dev/shm: %v", err)
		}
	})

	command := strings.Join([]string{
		"cat " + shellQuote(filepath.Join(denied, "readable")),
		"touch " + shellQuote(filepath.Join(allowed, "direct")),
		"bash -c " + shellQuote("touch "+shellQuote(filepath.Join(allowed, "child"))),
		"test ! -w " + shellQuote(denied),
		"! touch " + shellQuote(filepath.Join(denied, "blocked")),
		": >/dev/null",
		"touch /dev/shm/" + devShmName + " 2>/dev/null || true",
		"printf '%s' \"$COAGENT_BWRAP_TEST\"",
	}, " && ")

	t.Setenv("COAGENT_BWRAP_TEST", "inherited")
	cmd, err := runner.BashCommand(context.Background(), command, denied)
	require.NoError(t, err)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.Contains(t, string(output), "host data")
	assert.Contains(t, string(output), "inherited")
	assert.FileExists(t, filepath.Join(allowed, "direct"))
	assert.FileExists(t, filepath.Join(allowed, "child"))
	assert.NoFileExists(t, filepath.Join(denied, "blocked"))
	assert.NoFileExists(t, filepath.Join("/dev/shm", devShmName))
}

func TestBubblewrapShieldedLauncherDoesNotLoadModelEnvironment(t *testing.T) {
	if _, err := exec.LookPath(bubblewrapExecutable); err != nil {
		t.Skip("bwrap is not installed")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler is not installed")
	}

	base := t.TempDir()
	project := filepath.Join(base, "project")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.Mkdir(project, 0o755))
	require.NoError(t, os.Mkdir(outside, 0o755))
	library := filepath.Join(outside, "preload.so")
	source := filepath.Join(outside, "preload.c")
	require.NoError(t, os.WriteFile(source, []byte(`#include <stdio.h>
#include <stdlib.h>
__attribute__((constructor)) static void mark(void) {
  const char *path = getenv("COAGENT_PRELOAD_SENTINEL");
  if (path != NULL) { FILE *f = fopen(path, "w"); if (f != NULL) { fputs("loaded", f); fclose(f); } }
}
`), 0o600))
	output, err := exec.Command(cc, "-shared", "-fPIC", "-o", library, source).CombinedOutput()
	require.NoError(t, err, string(output))

	runner, err := New(Config{
		Enabled: true, WorkDir: project, SessionKey: "preload-test", ReadScope: ProjectConfined,
	}, nil)
	require.NoError(t, err)
	sentinel := filepath.Join(outside, "preload-ran")
	cmd, err := runner.Command(t.Context(), procexec.Request{
		Path: "/bin/sh", Args: []string{"-c", `test "$INNER_VALUE" = retained`}, WorkDir: project,
		Env: []string{
			"PATH=/usr/bin:/bin", "INNER_VALUE=retained", "LD_PRELOAD=" + library,
			"COAGENT_PRELOAD_SENTINEL=" + sentinel,
		},
	})
	require.NoError(t, err)
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.NoFileExists(t, sentinel)
	assert.Equal(t, sandboxLauncherEnvironment(), cmd.Env)
}

func TestBubblewrapShieldedIntegration(t *testing.T) {
	if _, err := exec.LookPath(bubblewrapExecutable); err != nil {
		t.Skip("bwrap is not installed")
	}

	base := t.TempDir()
	project := filepath.Join(base, "project")
	outside := filepath.Join(base, "outside-secret")
	require.NoError(t, os.Mkdir(project, 0o755))
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))

	runner, err := New(Config{
		Enabled: true, WorkDir: project, SessionKey: "shielded-test", ReadScope: ProjectConfined,
	}, nil)
	require.NoError(t, err)

	command := strings.Join([]string{
		"printf project > project-file",
		"grep -q project project-file",
		"test ! -e " + shellQuote(outside),
		"test ! -w /tmp",
	}, " && ")
	cmd, err := runner.BashCommand(context.Background(), command, project)
	require.NoError(t, err)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.FileExists(t, filepath.Join(project, "project-file"))
}

func TestBubblewrapProbeConfirmsEnforcement(t *testing.T) {
	if _, err := exec.LookPath(bubblewrapExecutable); err != nil {
		t.Skip("bwrap is not installed")
	}

	require.NoError(t, probeEnforcement(func(policy processPolicy) (Runner, error) {
		return newEnabledRunner(policy)
	}))
}

func TestBubblewrapShieldProbeConfirmsEnforcement(t *testing.T) {
	if _, err := exec.LookPath(bubblewrapExecutable); err != nil {
		t.Skip("bwrap is not installed")
	}

	require.NoError(t, probeShieldEnforcement(func(policy processPolicy) (Runner, error) {
		return newEnabledRunner(policy)
	}))
}

func TestBubblewrapIntegrationProtectsNestedMount(t *testing.T) {
	if os.Getenv("COAGENT_BWRAP_MOUNT_INTEGRATION") != "1" {
		t.Skip("set COAGENT_BWRAP_MOUNT_INTEGRATION=1 in a disposable Linux VM")
	}
	require.Zero(t, os.Geteuid(), "nested-mount integration requires root")

	if _, err := exec.LookPath(bubblewrapExecutable); err != nil {
		t.Skip("bwrap is not installed")
	}

	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	nested := filepath.Join(allowed, "nested-mount")
	require.NoError(t, os.MkdirAll(nested, 0o755))

	output, err := exec.Command("mount", "-t", "tmpfs", "tmpfs", nested).CombinedOutput()
	require.NoError(t, err, string(output))
	t.Cleanup(func() {
		if out, err := exec.Command("umount", nested).CombinedOutput(); err != nil {
			t.Errorf("unmount nested tmpfs: %v: %s", err, out)
		}
	})

	runner, err := newEnabledRunner(processPolicy{
		readScope: HostReadable, workDir: allowed, projectRoot: allowed,
		writableRoots: []string{allowed}, sessionKey: "test",
	})
	require.NoError(t, err)

	outerFile := filepath.Join(allowed, "outer-write")
	nestedFile := filepath.Join(nested, "blocked-write")
	command := "touch " + shellQuote(outerFile) + " && ! touch " + shellQuote(nestedFile)
	cmd, err := runner.BashCommand(context.Background(), command, allowed)
	require.NoError(t, err)
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.FileExists(t, outerFile)
	assert.NoFileExists(t, nestedFile)

	require.NoError(t, os.WriteFile(filepath.Join(nested, "readable"), []byte("nested"), 0o600))
	mounts, err := executionSubstrate()
	require.NoError(t, err)
	shielded, err := newEnabledRunner(processPolicy{
		readScope: ProjectConfined, workDir: allowed, projectRoot: allowed,
		writableRoots: []string{allowed}, readMounts: mounts, sessionKey: "nested-shielded",
	})
	require.NoError(t, err)
	command = "grep -q nested " + shellQuote(filepath.Join(nested, "readable")) +
		" && ! touch " + shellQuote(filepath.Join(nested, "shielded-blocked"))
	cmd, err = shielded.BashCommand(t.Context(), command, allowed)
	require.NoError(t, err)
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.NoFileExists(t, filepath.Join(nested, "shielded-blocked"))
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
