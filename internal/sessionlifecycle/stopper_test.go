package sessionlifecycle

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func TestStopperOwnsTreeFenceAndTerminalStatuses(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "session.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	_, err = db.ExecContext(ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test")
	require.NoError(t, err)

	sessions := sessionstore.NewStore(db)
	root, err := sessions.CreateSession(ctx, 1, "model", "", nil)
	require.NoError(t, err)
	childID, err := sessions.CreateSubagentSession(ctx, 1, root.ID, root.ID, "general", "model", "")
	require.NoError(t, err)

	stopper := NewStopper(sessions, sessions, sessions, subagent.NewStore(db))
	plan, err := stopper.Begin(ctx, root.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []int64{root.ID, childID}, plan.SessionIDs())

	rootAfterBegin, err := sessions.GetSession(ctx, root.ID)
	require.NoError(t, err)
	childAfterBegin, err := sessions.GetSession(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopping, rootAfterBegin.Status)
	assert.Equal(t, sessionstore.SessionStatusStopping, childAfterBegin.Status)

	require.NoError(t, stopper.Finish(ctx, plan, true))
	rootAfterFinish, err := sessions.GetSession(ctx, root.ID)
	require.NoError(t, err)
	childAfterFinish, err := sessions.GetSession(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopping, rootAfterFinish.Status)
	assert.Equal(t, sessionstore.SessionStatusStopped, childAfterFinish.Status)
}
