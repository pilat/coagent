package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	s, _ := newTestStoreWithSchedule(t)
	return s
}

func newTestStoreWithSchedule(t *testing.T) (Store, schedule.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))
	return NewStore(db), schedule.NewStore(db)
}

func testProject(t *testing.T, s Store, workDir string) int64 {
	t.Helper()
	pid, err := s.GetOrCreateProject(context.Background(), workDir)
	require.NoError(t, err)
	return pid
}

func TestStore_GetOrCreateProject(t *testing.T) {
	t.Run("creates and returns stable ID", func(t *testing.T) {
		s := newTestStore(t)

		id1, err := s.GetOrCreateProject(context.Background(), "/tmp/project")
		require.NoError(t, err)
		assert.Positive(t, id1)

		id2, err := s.GetOrCreateProject(context.Background(), "/tmp/project")
		require.NoError(t, err)
		assert.Equal(t, id1, id2, "same workdir should return same project ID")
	})

	t.Run("different workdirs get different IDs", func(t *testing.T) {
		s := newTestStore(t)

		id1, err := s.GetOrCreateProject(context.Background(), "/tmp/a")
		require.NoError(t, err)
		id2, err := s.GetOrCreateProject(context.Background(), "/tmp/b")
		require.NoError(t, err)
		assert.NotEqual(t, id1, id2)
	})

	t.Run("GetProjectWorkDir reverse lookup", func(t *testing.T) {
		s := newTestStore(t)

		pid, err := s.GetOrCreateProject(context.Background(), "/tmp/project")
		require.NoError(t, err)

		workDir, err := s.GetProjectWorkDir(context.Background(), pid)
		require.NoError(t, err)
		assert.Contains(t, workDir, "/tmp/project")
	})
}
