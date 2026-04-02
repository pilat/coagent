package sessionstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLatestActivityByProject(t *testing.T) {
	store, db, pid := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	pid2 := insertProject(t, db, "/tmp/p2", "p2")
	pid3 := insertProject(t, db, "/tmp/p3", "p3") // no sessions

	add := func(p int64, ts time.Time, killed bool) {
		rec, err := store.CreateSession(ctx, p, "m", "", nil)
		require.NoError(t, err)

		var execErr error
		if killed {
			_, execErr = db.ExecContext(ctx, `UPDATE sessions SET updated_at=?, killed_at=? WHERE id=?`, ts, ts, rec.ID)
		} else {
			_, execErr = db.ExecContext(ctx, `UPDATE sessions SET updated_at=? WHERE id=?`, ts, rec.ID)
		}

		require.NoError(t, execErr)
	}

	// pid: a newer killed session must be excluded in favor of the older live one.
	add(pid, base, true)
	add(pid, base.Add(-time.Hour), false)
	// pid2: every session killed → fall back to the newest killed.
	add(pid2, base.Add(-2*time.Hour), true)

	got, err := store.LatestActivityByProject(ctx, []int64{pid, pid2, pid3})
	require.NoError(t, err)

	require.Contains(t, got, pid)
	assert.True(t, got[pid].Equal(base.Add(-time.Hour)), "killed session excluded")
	require.Contains(t, got, pid2)
	assert.True(t, got[pid2].Equal(base.Add(-2*time.Hour)), "all-killed fallback")

	_, ok := got[pid3]
	assert.False(t, ok, "no-session project absent from map")
}

func TestLatestActivityByProject_Empty(t *testing.T) {
	store, _, _ := newTestStore(t)

	got, err := store.LatestActivityByProject(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func insertProject(t *testing.T, db *sql.DB, workDir, name string) int64 {
	t.Helper()

	res, err := db.ExecContext(
		context.Background(),
		`INSERT INTO projects (work_dir, name) VALUES (?, ?)`,
		workDir,
		name,
	)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)

	return id
}
