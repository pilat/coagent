//go:build linux || darwin

package builtin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/shellenv"
)

// TestBashTool_ShellEnvActivation proves the bash tool runs commands with the
// per-cwd activated shell env under BOTH sandbox modes. Uses a synthetic HOME rc
// that keys off the working directory (the way mise/direnv do); gated because it
// spawns a real interactive login shell.
func TestBashTool_ShellEnvActivation(t *testing.T) {
	if os.Getenv("COAGENT_SHELLENV_INTEGRATION") == "" {
		t.Skip("set COAGENT_SHELLENV_INTEGRATION=1 to run the interactive-shell integration test")
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}

	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(". ~/.bashrc\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".bashrc"),
		[]byte("[ -f ./marker ] && export COAGENT_MARKER=\"$(cat ./marker)\"\n"), 0o600))

	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("SHELL", bash)

	provider := shellenv.New()
	t.Cleanup(func() { _ = provider.Close() })

	for _, enabled := range []bool{false, true} {
		name := "sandbox_disabled"
		if enabled {
			name = "sandbox_enabled"
		}

		t.Run(name, func(t *testing.T) {
			if enabled && runtime.GOOS == "linux" {
				if _, err := exec.LookPath("bwrap"); err != nil {
					t.Skip("bwrap is not installed")
				}
			}

			wd := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(wd, "marker"), []byte("PINNED"), 0o600))

			runner, err := bashsandbox.New(bashsandbox.Config{Enabled: enabled, WorkDir: wd}, provider)
			require.NoError(t, err)

			params, err := json.Marshal(bashParams{Command: "printf '%s' \"$COAGENT_MARKER\"", WorkDir: wd})
			require.NoError(t, err)

			res, err := newBashTool(wd, runner).Execute(context.Background(), params)
			require.NoError(t, err)
			assert.Equal(t, "PINNED", res.Output, "bash tool must see the per-cwd activated env")
		})
	}
}
