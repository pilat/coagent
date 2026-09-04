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

	runner, err := newEnabledRunner([]string{allowed}, nil)
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

	cmd, err := runner.Command(context.Background(), command, denied)
	require.NoError(t, err)
	cmd.Env = append(os.Environ(), "COAGENT_BWRAP_TEST=inherited")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.Contains(t, string(output), "host data")
	assert.Contains(t, string(output), "inherited")
	assert.FileExists(t, filepath.Join(allowed, "direct"))
	assert.FileExists(t, filepath.Join(allowed, "child"))
	assert.NoFileExists(t, filepath.Join(denied, "blocked"))
	assert.NoFileExists(t, filepath.Join("/dev/shm", devShmName))
}

func TestBubblewrapProbeConfirmsEnforcement(t *testing.T) {
	if _, err := exec.LookPath(bubblewrapExecutable); err != nil {
		t.Skip("bwrap is not installed")
	}

	require.NoError(t, probeEnforcement(func(writable, readOnly []string) (Runner, error) {
		return newEnabledRunner(writable, readOnly)
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

	runner, err := newEnabledRunner([]string{allowed}, nil)
	require.NoError(t, err)

	outerFile := filepath.Join(allowed, "outer-write")
	nestedFile := filepath.Join(nested, "blocked-write")
	command := "touch " + shellQuote(outerFile) + " && ! touch " + shellQuote(nestedFile)
	cmd, err := runner.Command(context.Background(), command, allowed)
	require.NoError(t, err)
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.FileExists(t, outerFile)
	assert.NoFileExists(t, nestedFile)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
