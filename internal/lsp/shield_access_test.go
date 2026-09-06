package lsp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/safefile"
)

func TestManager_ShieldedAccessRejectsOutsideDocument(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	require.NoError(t, os.Mkdir(project, 0o755))
	outside := filepath.Join(base, "outside.go")
	require.NoError(t, os.WriteFile(outside, []byte("package outside\n"), 0o600))

	access, err := safefile.New(project, safefile.ProjectConfined)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, access.Close()) })
	manager := NewManagerWithAccess(nil, nil, access)
	t.Cleanup(manager.Close)

	_, err = manager.Definition(context.Background(), project, outside, 0, 0)
	require.Error(t, err)
	assert.ErrorContains(t, err, safefile.ShieldDeniedMessage)
}
