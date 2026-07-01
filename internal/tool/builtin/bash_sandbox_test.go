//go:build darwin || linux

package builtin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/bashsandbox"
)

func TestBashTool_TimeoutKillsDescendants(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
	}{
		{name: "disabled"},
		{name: "native sandbox", enabled: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.enabled && runtime.GOOS == "linux" {
				if _, err := exec.LookPath("bwrap"); err != nil {
					t.Skip("bwrap is not installed")
				}
			}

			workDir := t.TempDir()
			runner, err := bashsandbox.New(bashsandbox.Config{Enabled: tt.enabled, WorkDir: workDir}, nil)
			require.NoError(t, err)

			marker := filepath.Join(workDir, "descendant-writes")
			command := "while true; do printf x >>" + quoteShell(marker) + "; sleep 0.02; done & wait"
			params, err := json.Marshal(bashParams{Command: command, Timeout: 150})
			require.NoError(t, err)

			result, err := newBashTool(workDir, runner).Execute(context.Background(), params)
			require.NoError(t, err)
			assert.Equal(t, true, result.Metadata[metaKeyTimedOut])

			before, err := os.Stat(marker)
			require.NoError(t, err)
			time.Sleep(150 * time.Millisecond)
			after, err := os.Stat(marker)
			require.NoError(t, err)
			assert.Equal(t, before.Size(), after.Size(), "descendant survived timeout")
		})
	}
}

// TestBashTool_SandboxHintOnDeniedWrite drives the real backend end to end: a
// write outside the writable roots must fail AND carry the self-diagnosis hint.
func TestBashTool_SandboxHintOnDeniedWrite(t *testing.T) {
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("bwrap is not installed")
		}
	}

	deniedRoot, err := os.MkdirTemp(".", ".coagent-sandbox-hint-test-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(deniedRoot)) })
	deniedRoot, err = filepath.Abs(deniedRoot)
	require.NoError(t, err)

	workDir := t.TempDir()
	runner, err := bashsandbox.New(bashsandbox.Config{Enabled: true, WorkDir: workDir}, nil)
	require.NoError(t, err)

	target := filepath.Join(deniedRoot, "probe")

	params, err := json.Marshal(bashParams{Command: "touch " + quoteShell(target)})
	require.NoError(t, err)

	result, err := newBashTool(workDir, runner).Execute(context.Background(), params)
	require.NoError(t, err)

	assert.NotEqual(t, 0, result.Metadata[metaKeyExitCode])
	assert.Contains(t, result.Output, "tools.bash.sandbox.writable_paths")
	assert.NoFileExists(t, target)
}

func quoteShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
