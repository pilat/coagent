package builtin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyPatchHunklessProjectionIsEmpty(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("untouched"), 0o600))
	candidate := newApplyPatchToolWithAccess(dir, nil, directFileMutator{}, nil)
	calls, err := candidate.HookCalls(t.Context(), marshalApplyPatchParams(t, "--- a/file\n+++ b/file\n"))
	require.NoError(t, err)
	assert.Empty(t, calls)
}
