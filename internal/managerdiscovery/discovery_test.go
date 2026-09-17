package managerdiscovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

type hiddenDirBackend struct {
	hidden []string
}

func (b *hiddenDirBackend) GetSession(context.Context, int64) (*sessionstore.SessionRecord, error) {
	return nil, nil
}

func (b *hiddenDirBackend) GetOrCreateProject(context.Context, string) (int64, error) {
	return 0, nil
}

func (b *hiddenDirBackend) GetOrCreateHiddenProject(context.Context, string) (int64, error) {
	return 0, nil
}

func (b *hiddenDirBackend) GetProjectWorkDir(context.Context, int64) (string, error) {
	return "", nil
}

func (b *hiddenDirBackend) ListHiddenProjectDirs(context.Context) ([]string, error) {
	return b.hidden, nil
}

func (b *hiddenDirBackend) ListRecentProjects(context.Context, string) ([]controllerapi.RecentProjectInfo, error) {
	return nil, nil
}

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

	svc := &service{backend: &hiddenDirBackend{hidden: []string{hidden}}}

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
