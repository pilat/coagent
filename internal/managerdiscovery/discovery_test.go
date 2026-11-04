package managerdiscovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

// Only the directory backed by a hidden project row is omitted; an unrelated
// same-named directory elsewhere stays navigable.
func TestListDir_OmitsHiddenProjectDirByPath(t *testing.T) {
	root := t.TempDir()
	hidden := filepath.Join(root, "projects", "sys_coagent")
	visible := filepath.Join(root, "projects")
	sibling := filepath.Join(root, "other", "sys_coagent")

	for _, dir := range []string{hidden, filepath.Join(visible, "blog"), sibling} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	store, db := newProjectTestManager(t)
	_, err := db.ExecContext(
		t.Context(),
		`INSERT INTO projects(work_dir, name, hidden) VALUES (?, ?, TRUE)`,
		hidden,
		filepath.Base(hidden),
	)
	require.NoError(t, err)
	svc := &Service{backend: store}

	result, err := svc.ListDir(context.Background(), controllerapi.FsListDirData{
		Path: filepath.Join(root, "projects"),
	})
	require.NoError(t, err)

	names := make([]string, 0, len(result.Dirs))
	for _, dir := range result.Dirs {
		names = append(names, dir.Name)
	}

	assert.Contains(t, names, "blog")
	assert.NotContains(t, names, "sys_coagent")

	siblingResult, err := svc.ListDir(context.Background(), controllerapi.FsListDirData{
		Path: filepath.Join(root, "other"),
	})
	require.NoError(t, err)
	require.Len(t, siblingResult.Dirs, 1)
	assert.Equal(t, "sys_coagent", siblingResult.Dirs[0].Name)
}
