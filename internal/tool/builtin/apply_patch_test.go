package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyPatchTool_CreateRelativeFile(t *testing.T) {
	workDir := t.TempDir()
	tool := newApplyPatchTool(workDir, directFileMutator{})
	params := marshalApplyPatchParams(t, `--- /dev/null
+++ b/nested/new.txt
@@ -0,0 +1,1 @@
+created`)

	result, err := tool.Execute(context.Background(), params)

	require.NoError(t, err)
	assert.Contains(t, result.Output, "nested/new.txt")
	content, err := os.ReadFile(filepath.Join(workDir, "nested", "new.txt"))
	require.NoError(t, err)
	assert.Equal(t, "created", string(content))
}

func TestApplyPatchTool_UpdatesExistingFile(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("before\n"), 0o644))
	tool := newApplyPatchTool(workDir, directFileMutator{})
	params := marshalApplyPatchParams(t, `--- a/file.txt
+++ b/file.txt
@@ -1,1 +1,1 @@
-before
+after`)

	_, err := tool.Execute(context.Background(), params)

	require.NoError(t, err)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "after\n", string(content))
}

func TestApplyPatchTool_MultipleFilesRemainSequential(t *testing.T) {
	workDir := t.TempDir()
	tool := newApplyPatchTool(workDir, directFileMutator{})
	params := marshalApplyPatchParams(t, `--- /dev/null
+++ b/one.txt
@@ -0,0 +1,1 @@
+one
--- /dev/null
+++ b/two.txt
@@ -0,0 +1,1 @@
+two`)

	_, err := tool.Execute(context.Background(), params)

	require.NoError(t, err)
	one, err := os.ReadFile(filepath.Join(workDir, "one.txt"))
	require.NoError(t, err)
	two, err := os.ReadFile(filepath.Join(workDir, "two.txt"))
	require.NoError(t, err)
	assert.Equal(t, "one", string(one))
	assert.Equal(t, "two", string(two))
}

func TestApplyPatchTool_RejectsEmptyAndMalformedPatch(t *testing.T) {
	tool := newApplyPatchTool(t.TempDir(), directFileMutator{})

	for name, patch := range map[string]string{
		"empty":     "",
		"malformed": "not a unified patch",
	} {
		t.Run(name, func(t *testing.T) {
			params := marshalApplyPatchParams(t, patch)
			result, err := tool.Execute(context.Background(), params)

			require.Error(t, err)
			assert.Nil(t, result)
		})
	}
}

func TestApplyPatchTool_DelegatesMutation(t *testing.T) {
	want := errors.New("patch denied")
	mutator := &recordingFileMutator{err: want}
	workDir := t.TempDir()
	path := filepath.Join(workDir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("before\n"), 0o644))
	tool := newApplyPatchTool(workDir, mutator)
	params := marshalApplyPatchParams(t, `--- a/file.txt
+++ b/file.txt
@@ -1,1 +1,1 @@
-before
+after`)

	ctx := context.WithValue(context.Background(), mutationContextKey{}, "marker")
	result, err := tool.Execute(ctx, params)

	require.ErrorIs(t, err, want)
	assert.Nil(t, result)
	require.Len(t, mutator.calls, 1)
	assert.Equal(t, path, mutator.calls[0].path)
	assert.Equal(t, []byte("after\n"), mutator.calls[0].content)
	assert.True(t, mutator.calls[0].createParents)
	assert.Equal(t, "marker", mutator.calls[0].ctx.Value(mutationContextKey{}))

	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "before\n", string(content))
}

func marshalApplyPatchParams(t *testing.T, patch string) json.RawMessage {
	t.Helper()

	params, err := json.Marshal(applyPatchParams{Patch: patch})
	require.NoError(t, err, "marshal patch %q", patch)

	return params
}
