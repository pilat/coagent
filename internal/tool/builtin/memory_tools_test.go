package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/memory"
	"github.com/pilat/coagent/internal/migrate"
)

type fakeCuratedStore struct {
	count         int
	countErr      error
	saveID        int64
	saveErr       error
	saved         []string
	deleted       []int64
	deletedScopes []int64
	deleteErr     error
	remaining     []memory.MemoryEntry
	listErr       error
}

var _ memory.CuratedStore = (*fakeCuratedStore)(nil)

func (s *fakeCuratedStore) SaveMemory(_ context.Context, _ int64, text string) (int64, error) {
	s.saved = append(s.saved, text)
	return s.saveID, s.saveErr
}

func (s *fakeCuratedStore) DeleteMemory(_ context.Context, projectID, id int64) error {
	s.deleted = append(s.deleted, id)
	s.deletedScopes = append(s.deletedScopes, projectID)

	return s.deleteErr
}

func (s *fakeCuratedStore) ListMemoryTexts(_ context.Context, _ int64) ([]memory.MemoryEntry, error) {
	return s.remaining, s.listErr
}

func (s *fakeCuratedStore) CountMemories(_ context.Context, _ int64) (int, error) {
	return s.count, s.countErr
}

func (s *fakeCuratedStore) ListMemories(_ context.Context, _ int64) ([]memory.CuratedMemory, error) {
	return nil, nil
}

func newRealCuratedStore(t *testing.T) (memory.CuratedStore, func(workDir string) int64) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))

	createProject := func(workDir string) int64 {
		t.Helper()
		res, err := db.Exec(`INSERT INTO projects (work_dir, name) VALUES (?, ?)`, workDir, filepath.Base(workDir))
		require.NoError(t, err)
		id, err := res.LastInsertId()
		require.NoError(t, err)

		return id
	}

	return memory.NewCuratedStore(db), createProject
}

func TestMemoryDeleteRefusesForeignProjectAgainstRealStore(t *testing.T) {
	store, createProject := newRealCuratedStore(t)
	pidA := createProject("/tmp/project-a")
	pidB := createProject("/tmp/project-b")

	idB, err := store.SaveMemory(context.Background(), pidB, "memory for B")
	require.NoError(t, err)

	raw, err := json.Marshal(memoryDeleteParams{ID: idB})
	require.NoError(t, err)

	changed := 0
	_, err = NewMemoryDeleteTool(store, pidA, func(context.Context) { changed++ }).Execute(context.Background(), raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Zero(t, changed, "a refused delete must not refresh the prompt")

	remaining, err := store.ListMemories(context.Background(), pidB)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "project B's memory must survive")
}

func TestMemorySaveTextLengthBoundary(t *testing.T) {
	tests := []struct {
		name      string
		length    int
		wantSaved bool
		wantText  string
	}{
		{name: "exactly at the cap is saved", length: memoryMaxTextLen, wantSaved: true, wantText: "Saved memory 7"},
		{
			name:      "one over the cap is refused",
			length:    memoryMaxTextLen + 1,
			wantSaved: false,
			wantText:  "Text too long",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeCuratedStore{saveID: 7}
			saveTool := NewMemorySaveTool(store, 1, nil)

			raw, err := json.Marshal(memorySaveParams{Text: strings.Repeat("m", tt.length)})
			require.NoError(t, err)

			result, err := saveTool.Execute(context.Background(), raw)
			require.NoError(t, err)

			assert.Contains(t, result.Output, tt.wantText)
			assert.Equal(t, tt.wantSaved, len(store.saved) == 1)
		})
	}
}

func TestMemorySaveCountBoundary(t *testing.T) {
	tests := []struct {
		name      string
		count     int
		wantSaved bool
		wantText  string
	}{
		{name: "one below the cap is saved", count: memoryMaxCount - 1, wantSaved: true, wantText: "(50/50)"},
		{
			name:      "at the cap is refused",
			count:     memoryMaxCount,
			wantSaved: false,
			wantText:  "Memory limit reached (50/50)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeCuratedStore{count: tt.count, saveID: 3}

			changed := 0
			saveTool := NewMemorySaveTool(store, 1, func(context.Context) { changed++ })

			result, err := saveTool.Execute(context.Background(), json.RawMessage(`{"text":"note"}`))
			require.NoError(t, err)

			assert.Contains(t, result.Output, tt.wantText)
			assert.Equal(t, tt.wantSaved, len(store.saved) == 1)
			assert.Equal(t, tt.wantSaved, changed == 1, "the prompt refresh must follow a real save")
		})
	}
}

func TestMemorySaveSurfacesStoreErrors(t *testing.T) {
	tests := []struct {
		name    string
		store   *fakeCuratedStore
		raw     string
		wantErr string
	}{
		{name: "malformed json", store: &fakeCuratedStore{}, raw: `{`, wantErr: "invalid parameters"},
		{name: "empty text", store: &fakeCuratedStore{}, raw: `{"text":""}`, wantErr: "text is required"},
		{
			name:    "count fails",
			store:   &fakeCuratedStore{countErr: errors.New("db down")},
			raw:     `{"text":"note"}`,
			wantErr: "count memories: db down",
		},
		{
			name:    "save fails",
			store:   &fakeCuratedStore{saveErr: errors.New("disk full")},
			raw:     `{"text":"note"}`,
			wantErr: "save memory: disk full",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewMemorySaveTool(tt.store, 1, nil).Execute(context.Background(), json.RawMessage(tt.raw))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestMemoryDeleteListsRemaining(t *testing.T) {
	store := &fakeCuratedStore{remaining: []memory.MemoryEntry{{ID: 2, Text: "kept"}}}

	changed := 0
	result, err := NewMemoryDeleteTool(store, 42, func(context.Context) { changed++ }).
		Execute(context.Background(), json.RawMessage(`{"id":5}`))
	require.NoError(t, err)

	assert.Equal(t, []int64{5}, store.deleted)
	assert.Equal(t, []int64{42}, store.deletedScopes, "the delete must carry the session's project scope")
	assert.Equal(t, 1, changed)
	assert.Equal(t, "Deleted memory 5. Remaining memories (1):\n- [2] kept\n", result.Output)
}

func TestMemoryDeleteReportsEmptyRemainder(t *testing.T) {
	result, err := NewMemoryDeleteTool(&fakeCuratedStore{}, 1, nil).
		Execute(context.Background(), json.RawMessage(`{"id":5}`))
	require.NoError(t, err)

	assert.Equal(t, "Deleted memory 5. Remaining memories (0):\n(none)", result.Output)
}

func TestMemoryDeleteReportsListFailureWithoutFailingTheDelete(t *testing.T) {
	store := &fakeCuratedStore{listErr: errors.New("db down")}

	result, err := NewMemoryDeleteTool(store, 1, nil).
		Execute(context.Background(), json.RawMessage(`{"id":5}`))
	require.NoError(t, err)

	assert.Equal(t, []int64{5}, store.deleted)
	assert.Contains(t, result.Output, "Deleted memory 5. (Could not list remaining: db down)")
}

func TestMemoryDeleteSurfacesErrors(t *testing.T) {
	tests := []struct {
		name    string
		store   *fakeCuratedStore
		raw     string
		wantErr string
	}{
		{name: "malformed json", store: &fakeCuratedStore{}, raw: `{`, wantErr: "invalid parameters"},
		{name: "missing id", store: &fakeCuratedStore{}, raw: `{"id":0}`, wantErr: "id is required"},
		{
			name:    "delete fails",
			store:   &fakeCuratedStore{deleteErr: errors.New("locked")},
			raw:     `{"id":9}`,
			wantErr: "delete memory: locked",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewMemoryDeleteTool(tt.store, 1, nil).Execute(context.Background(), json.RawMessage(tt.raw))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
