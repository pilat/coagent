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

type silentFileMutator struct{}

type redirectedFileMutator struct {
	path string
}

func (silentFileMutator) WriteFile(context.Context, string, []byte, bool) error {
	return nil
}

func (m redirectedFileMutator) WriteFile(_ context.Context, _ string, content []byte, createParents bool) error {
	if createParents {
		if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
			return err
		}
	}

	return os.WriteFile(m.path, content, 0o644)
}

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

func TestApplyPatchTool_SandboxUpdatesTargetFile(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "tmp", "patch-bug-repro.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("alpha\nbeta"), 0o644))

	tool := newApplyPatchTool(workDir, &sandboxFileMutator{runner: &bashRunnerStub{}})
	params := marshalApplyPatchParams(t, `--- a/tmp/patch-bug-repro.txt
+++ b/tmp/patch-bug-repro.txt
@@ -1,2 +1,3 @@
 alpha
+inserted line
 beta`)

	result, err := tool.Execute(context.Background(), params)

	require.NoError(t, err)
	require.NotNil(t, result)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "alpha\ninserted line\nbeta", string(content))
}

func TestApplyPatchTool_RejectsSilentMutation(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o644))
	tool := newApplyPatchTool(workDir, silentFileMutator{})
	params := marshalApplyPatchParams(t, `--- a/file.txt
+++ b/file.txt
@@ -1,1 +1,1 @@
-before
+after`)

	result, err := tool.Execute(context.Background(), params)

	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "verify")
}

func TestApplyPatchTool_RejectsRedirectedMutation(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "file.txt")
	redirect := filepath.Join(workDir, "redirected.txt")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o644))
	tool := newApplyPatchTool(workDir, redirectedFileMutator{path: redirect})
	params := marshalApplyPatchParams(t, `--- a/file.txt
+++ b/file.txt
@@ -1,1 +1,1 @@
-before
+after`)

	result, err := tool.Execute(context.Background(), params)

	require.Error(t, err)
	assert.Nil(t, result)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "before", string(content))
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

func TestApplyPatchTool_PlansAllFilesBeforeWriting(t *testing.T) {
	workDir := t.TempDir()
	first := filepath.Join(workDir, "first.txt")
	require.NoError(t, os.WriteFile(first, []byte("before\n"), 0o644))
	tool := newApplyPatchTool(workDir, directFileMutator{})
	patch := "--- a/first.txt\n+++ b/first.txt\n@@ -1,1 +1,1 @@\n-before\n+after\n" +
		"--- a/second.txt\n+++ b/second.txt\n@@ -1,1 +1,1 @@\n-missing\n+new"

	result, err := tool.Execute(context.Background(), marshalApplyPatchParams(t, patch))

	require.Error(t, err)
	assert.Nil(t, result)
	content, readErr := os.ReadFile(first)
	require.NoError(t, readErr)
	assert.Equal(t, "before\n", string(content))
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
	require.Len(t, mutator.calls, 2)
	assert.Equal(t, path, mutator.calls[0].path)
	assert.Equal(t, []byte("after\n"), mutator.calls[0].content)
	assert.True(t, mutator.calls[0].createParents)
	assert.Equal(t, "marker", mutator.calls[0].ctx.Value(mutationContextKey{}))
	assert.Equal(t, path, mutator.calls[1].path)
	assert.Equal(t, []byte("before\n"), mutator.calls[1].content)

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
