package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

func TestFilesystemTools_DisabledSandboxPreservesOutsideWrites(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	require.NoError(t, os.Mkdir(workDir, 0o755))
	require.NoError(t, os.Mkdir(outside, 0o755))
	require.False(t, disabledTestPathWithin(outside, workDir))

	stack, err := BuildStack(context.Background(), StackConfig{
		WorkDir: workDir,
		Loader:  loader.New(),
		Todo:    todo.New(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stack.Close()) })

	for _, toolID := range []string{"write", "edit", "apply_patch"} {
		t.Run(toolID, func(t *testing.T) {
			path := filepath.Join(outside, toolID+"-disabled.txt")
			require.NoError(t, os.WriteFile(path, []byte("before"), 0o644))

			require.NoError(t, executeDisabledToolMutation(t, stack.Registry.Get(toolID), path))
			content, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, "after", string(content))
		})
	}
}

func executeDisabledToolMutation(t *testing.T, impl tool.Tool, path string) error {
	t.Helper()

	var params any
	switch impl.ID() {
	case "write":
		params = writeParams{FilePath: path, Content: "after"}
	case "edit":
		params = editParams{FilePath: path, OldString: "before", NewString: "after"}
	case "apply_patch":
		params = applyPatchParams{Patch: fmt.Sprintf(
			"--- %s\n+++ %s\n@@ -1,1 +1,1 @@\n-before\n+after",
			path,
			path,
		)}
	default:
		t.Fatalf("unsupported mutation tool %q", impl.ID())
	}

	data, err := json.Marshal(params)
	require.NoError(t, err)
	_, err = impl.Execute(context.Background(), data)

	return err
}

func disabledTestPathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
