//go:build linux

package builtin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/tool"
)

func TestBashTool_TimeoutKillsDescendants(t *testing.T) {
	restore := coagenthome.Override(t.TempDir())
	defer restore()

	tests := []struct {
		name    string
		enabled bool
	}{
		{name: "disabled"},
		{name: "native sandbox", enabled: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.enabled {
				if _, err := exec.LookPath("bwrap"); err != nil {
					t.Skip("bwrap is not installed")
				}
			}

			workDir := t.TempDir()
			runner, err := bashsandbox.New(sandboxRunnerConfig(t, workDir, tt.enabled), nil)
			require.NoError(t, err)

			marker := filepath.Join(workDir, "descendant-writes")
			command := "while true; do printf x >>" + quoteShell(marker) + "; sleep 0.02; done & wait"
			// The sandbox builds a private root, proc and device set before bash
			// starts, so the budget must cover that setup and still leave time for
			// the descendant to write.
			params, err := json.Marshal(bashParams{Command: command, Timeout: 1500})
			require.NoError(t, err)

			result, err := newTestBashToolRunner(
				t,
				workDir,
				runner,
			).Execute(tool.WithCallID(context.Background(), "call_test"), params)
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
	restore := coagenthome.Override(t.TempDir())
	defer restore()

	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap is not installed")
	}

	readOnlyRoot := t.TempDir()

	workDir := t.TempDir()
	policy, err := bashsandbox.FixturePolicy(workDir, false)
	require.NoError(t, err)
	policy.Grants = append(policy.Grants, sandboxpolicy.Grant{
		Source: readOnlyRoot, Target: readOnlyRoot, Mode: sandboxpolicy.ModeReadOnly,
		Kind: sandboxpolicy.KindDir, Present: true,
	})
	runner, err := bashsandbox.New(
		bashsandbox.Config{Enabled: true, Policy: policy, WorkDir: workDir}, nil,
	)
	require.NoError(t, err)

	target := filepath.Join(readOnlyRoot, "probe")

	params, err := json.Marshal(bashParams{Command: "touch " + quoteShell(target)})
	require.NoError(t, err)

	result, err := newTestBashToolRunner(t, workDir, runner).
		Execute(tool.WithCallID(context.Background(), "call_hint"), params)
	require.NoError(t, err)

	assert.NotEqual(t, 0, result.Metadata[metaKeyExitCode])
	assert.Contains(t, result.Output, "sandbox.profiles")
	assert.NoFileExists(t, target)
}

func quoteShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// sandboxRunnerConfig builds the runner configuration these fixtures need: an
// enabled runner requires the compiled policy a session would have.
func sandboxRunnerConfig(t *testing.T, workDir string, enabled bool) bashsandbox.Config {
	t.Helper()

	cfg := bashsandbox.Config{Enabled: enabled, WorkDir: workDir}
	if !enabled {
		return cfg
	}

	policy, err := bashsandbox.FixturePolicy(workDir, false)
	require.NoError(t, err)
	cfg.Policy = policy

	return cfg
}
