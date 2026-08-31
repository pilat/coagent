package sessionstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
)

func newBaselineStore(t *testing.T) (*sql.DB, Store, int64) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "session.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	result, err := db.ExecContext(
		ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test",
	)
	require.NoError(t, err)

	projectID, err := result.LastInsertId()
	require.NoError(t, err)

	store := NewStore(db)
	record, err := store.CreateSession(ctx, projectID, "glm-5.3-flash", "", map[string]any{})
	require.NoError(t, err)

	return db, store, record.ID
}

func TestContextBaseline_SaveLoadClear(t *testing.T) {
	ctx := context.Background()
	_, store, sessionID := newBaselineStore(t)

	require.NoError(t, store.SaveContextBaseline(ctx, sessionID, ContextBaseline{
		Model: "glm-5.3-flash", PromptTokens: 199_453, MessageCount: 81,
	}))

	rec, err := store.GetSession(ctx, sessionID)
	require.NoError(t, err)

	got := rec.ContextBaseline()
	require.NotNil(t, got, "a saved measurement survives the round-trip")
	assert.Equal(t, "glm-5.3-flash", got.Model)
	assert.Equal(t, 199_453, got.PromptTokens)
	assert.Equal(t, 81, got.MessageCount)

	require.NoError(t, store.ClearContextBaseline(ctx, sessionID))

	rec, err = store.GetSession(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, rec.ContextBaseline(), "a cleared row reads as nothing measured")
}

// Rows written before the columns existed read as an empty baseline.
func TestContextBaseline_RowWithoutMeasurement(t *testing.T) {
	ctx := context.Background()
	_, store, sessionID := newBaselineStore(t)

	rec, err := store.GetSession(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, rec.ContextBaseline())
}

func TestContextBaseline_UnknownSessionRejected(t *testing.T) {
	ctx := context.Background()
	_, store, _ := newBaselineStore(t)

	err := store.SaveContextBaseline(ctx, 999_999, ContextBaseline{Model: "m", PromptTokens: 1, MessageCount: 1})
	require.Error(t, err)

	err = store.ClearContextBaseline(ctx, 999_999)
	require.Error(t, err)
}

// The baseline is derived bookkeeping: bumping updated_at on every successful
// response would float running sessions to the top of activity-ordered lists.
func TestContextBaseline_SaveDoesNotBumpUpdatedAt(t *testing.T) {
	ctx := context.Background()
	db, store, sessionID := newBaselineStore(t)

	var before sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, `SELECT updated_at FROM sessions WHERE id = ?`, sessionID).Scan(&before))

	require.NoError(t, store.SaveContextBaseline(ctx, sessionID, ContextBaseline{
		Model: "glm-5.3-flash", PromptTokens: 10, MessageCount: 2,
	}))

	var after sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, `SELECT updated_at FROM sessions WHERE id = ?`, sessionID).Scan(&after))

	assert.Equal(t, before.String, after.String, "updated_at is untouched by baseline writes")
}
