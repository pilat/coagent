package lsp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveFileConfinesLexically(t *testing.T) {
	workDir := t.TempDir()
	relative, err := resolveFile(workDir, "nested/file.go")
	require.NoError(t, err)
	abs, err := resolveFile(workDir, filepath.Join(workDir, "nested/file.go"))
	require.NoError(t, err)
	assert.Equal(t, relative, abs)
	_, err = resolveFile(workDir, "../outside.go")
	require.Error(t, err)
	_, err = resolveFile(workDir, workDir+"2/file.go")
	require.Error(t, err)
}

func TestRootMarkersObserveFileAndDirectoryKinds(t *testing.T) {
	workDir := t.TempDir()
	terraform := filepath.Join(workDir, ".terraform")
	require.NoError(t, os.Mkdir(terraform, 0o755))
	assert.True(t, containsRootMarker(workDir, []rootMarker{exactDir(".terraform")}))
	assert.False(t, containsRootMarker(workDir, []rootMarker{exactFile(".terraform")}))
}
