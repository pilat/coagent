package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSandboxHint(t *testing.T) {
	roots := []string{"/work", "/tmp"}

	tests := map[string]struct {
		output string
		roots  []string
		want   bool
	}{
		"bwrap read-only denial": {
			output: "go: open /home/u/go/pkg/mod/lock: Read-only file system",
			roots:  roots,
			want:   true,
		},
		"lowercase errno text": {
			output: "mkdir: cannot create directory: read-only file system",
			roots:  roots,
			want:   true,
		},
		"seatbelt denial":       {output: "touch: /etc/x: Operation not permitted", roots: roots, want: true},
		"unrelated failure":     {output: "compile error: undefined symbol", roots: roots, want: false},
		"denial but unconfined": {output: "Read-only file system", roots: nil, want: false},
		"empty output":          {output: "", roots: roots, want: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			hint := sandboxHint(tt.output, tt.roots)

			if !tt.want {
				assert.Empty(t, hint)
				return
			}

			require.NotEmpty(t, hint)
			assert.Contains(t, hint, "tools.bash.sandbox.writable_paths")
			assert.Contains(t, hint, "/work, /tmp")
		})
	}
}

func TestBashTool_SandboxHint(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("hints on write denial under confinement", func(t *testing.T) {
		tool := newBashTool(tmpDir, &bashRunnerStub{roots: []string{tmpDir, "/tmp"}})

		params, err := json.Marshal(bashParams{Command: "echo 'touch: /denied/x: Read-only file system' >&2; exit 1"})
		require.NoError(t, err)

		result, err := tool.Execute(context.Background(), params)
		require.NoError(t, err)

		assert.Equal(t, 1, result.Metadata["exitCode"])
		assert.Contains(t, result.Output, "tools.bash.sandbox.writable_paths")
		assert.Contains(t, result.Output, tmpDir)
	})

	t.Run("no hint on success even with marker in output", func(t *testing.T) {
		tool := newBashTool(tmpDir, &bashRunnerStub{roots: []string{tmpDir}})

		params, err := json.Marshal(bashParams{Command: "echo 'Read-only file system'"})
		require.NoError(t, err)

		result, err := tool.Execute(context.Background(), params)
		require.NoError(t, err)

		assert.NotContains(t, result.Output, "writable_paths")
	})

	t.Run("no hint when unconfined", func(t *testing.T) {
		tool := newBashTool(tmpDir, &bashRunnerStub{})

		params, err := json.Marshal(bashParams{Command: "echo 'Read-only file system' >&2; exit 1"})
		require.NoError(t, err)

		result, err := tool.Execute(context.Background(), params)
		require.NoError(t, err)

		assert.NotContains(t, result.Output, "writable_paths")
	})
}
