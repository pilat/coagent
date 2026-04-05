package memory

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
)

func newTestCuratedStore(t *testing.T) (*curatedStore, func(workDir string) int64) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	// Create minimal schema needed for curated store tests.
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS projects (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			work_dir TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS memories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL DEFAULT 0,
			text TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	} {
		_, err := db.Exec(ddl)
		require.NoError(t, err)
	}

	store := NewCuratedStore(db).(*curatedStore)

	createProject := func(workDir string) int64 {
		t.Helper()
		_, err := db.Exec(
			`INSERT OR IGNORE INTO projects (work_dir, name) VALUES (?, ?)`,
			workDir,
			filepath.Base(workDir),
		)
		require.NoError(t, err)
		var id int64
		require.NoError(t, db.QueryRow(`SELECT id FROM projects WHERE work_dir = ?`, workDir).Scan(&id))
		return id
	}

	return store, createProject
}

func TestCuratedStore_SaveAndList(t *testing.T) {
	s, createProject := newTestCuratedStore(t)
	pid := createProject("/tmp/project")

	id, err := s.SaveMemory(context.Background(), pid, "user prefers Go")
	require.NoError(t, err)
	assert.Positive(t, id)

	memories, err := s.ListMemories(context.Background(), pid)
	require.NoError(t, err)
	require.Len(t, memories, 1)
	assert.Equal(t, "user prefers Go", memories[0].Text)
}

func TestCuratedStore_ListEmpty(t *testing.T) {
	s, createProject := newTestCuratedStore(t)
	pid := createProject("/tmp/project")

	memories, err := s.ListMemories(context.Background(), pid)
	require.NoError(t, err)
	assert.Empty(t, memories)
}

func TestCuratedStore_SortedByCreatedAt(t *testing.T) {
	s, createProject := newTestCuratedStore(t)
	pid := createProject("/tmp/project")

	_, err := s.SaveMemory(context.Background(), pid, "first")
	require.NoError(t, err)
	_, err = s.SaveMemory(context.Background(), pid, "second")
	require.NoError(t, err)
	_, err = s.SaveMemory(context.Background(), pid, "third")
	require.NoError(t, err)

	memories, err := s.ListMemories(context.Background(), pid)
	require.NoError(t, err)
	require.Len(t, memories, 3)
	assert.Equal(t, "first", memories[0].Text)
	assert.Equal(t, "second", memories[1].Text)
	assert.Equal(t, "third", memories[2].Text)
}

func TestCuratedStore_ScopedByProject(t *testing.T) {
	s, createProject := newTestCuratedStore(t)
	pidA := createProject("/tmp/project-a")
	pidB := createProject("/tmp/project-b")

	_, err := s.SaveMemory(context.Background(), pidA, "memory for A")
	require.NoError(t, err)
	_, err = s.SaveMemory(context.Background(), pidB, "memory for B")
	require.NoError(t, err)

	memoriesA, err := s.ListMemories(context.Background(), pidA)
	require.NoError(t, err)
	require.Len(t, memoriesA, 1)
	assert.Equal(t, "memory for A", memoriesA[0].Text)

	memoriesB, err := s.ListMemories(context.Background(), pidB)
	require.NoError(t, err)
	require.Len(t, memoriesB, 1)
	assert.Equal(t, "memory for B", memoriesB[0].Text)
}

func TestCuratedStore_Delete(t *testing.T) {
	s, createProject := newTestCuratedStore(t)
	pid := createProject("/tmp/project")

	id, err := s.SaveMemory(context.Background(), pid, "to be deleted")
	require.NoError(t, err)

	require.NoError(t, s.DeleteMemory(context.Background(), pid, id))

	memories, err := s.ListMemories(context.Background(), pid)
	require.NoError(t, err)
	assert.Empty(t, memories)
}

func TestCuratedStore_DeleteNotFound(t *testing.T) {
	s, createProject := newTestCuratedStore(t)
	pid := createProject("/tmp/project")

	err := s.DeleteMemory(context.Background(), pid, 99999)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestCuratedStore_DeleteRefusesForeignProject(t *testing.T) {
	s, createProject := newTestCuratedStore(t)
	pidA := createProject("/tmp/project-a")
	pidB := createProject("/tmp/project-b")

	_, err := s.SaveMemory(context.Background(), pidA, "memory for A")
	require.NoError(t, err)
	idB, err := s.SaveMemory(context.Background(), pidB, "memory for B")
	require.NoError(t, err)

	err = s.DeleteMemory(context.Background(), pidA, idB)
	require.Error(t, err, "deleting project B's memory while scoped to A must fail")
	assert.Contains(t, err.Error(), "not found")

	memoriesB, err := s.ListMemories(context.Background(), pidB)
	require.NoError(t, err)
	require.Len(t, memoriesB, 1)
	assert.Equal(t, "memory for B", memoriesB[0].Text)

	require.NoError(t, s.DeleteMemory(context.Background(), pidB, idB), "the owner must still be able to delete it")
}

func TestCuratedStore_Count(t *testing.T) {
	s, createProject := newTestCuratedStore(t)
	pid := createProject("/tmp/project")

	count, err := s.CountMemories(context.Background(), pid)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	_, err = s.SaveMemory(context.Background(), pid, "one")
	require.NoError(t, err)
	_, err = s.SaveMemory(context.Background(), pid, "two")
	require.NoError(t, err)

	count, err = s.CountMemories(context.Background(), pid)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

func TestCuratedStore_ListMemoryTexts(t *testing.T) {
	s, createProject := newTestCuratedStore(t)
	pid := createProject("/tmp/project")

	_, err := s.SaveMemory(context.Background(), pid, "hello")
	require.NoError(t, err)

	entries, err := s.ListMemoryTexts(context.Background(), pid)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "hello", entries[0].Text)
	assert.Positive(t, entries[0].ID)
}
