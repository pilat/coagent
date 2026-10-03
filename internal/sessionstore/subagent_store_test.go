package sessionstore_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func newTestSubagentStore(db *sql.DB) subagent.Store {
	return subagent.NewStore(db, testStore(db))
}

func newTestStore(t *testing.T) (*sessionstore.Store, *sql.DB, int64) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { releaseFixtureDB(db); _ = db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))

	res, err := db.ExecContext(
		context.Background(),
		`INSERT INTO projects (work_dir, name) VALUES (?, ?)`,
		t.TempDir(), "test",
	)
	require.NoError(t, err)
	projectID, err := res.LastInsertId()
	require.NoError(t, err)

	return testStore(db), db, projectID
}

// seedLink inserts a bare row for tests that exercise a specific ledger state.
func seedLink(t *testing.T, db *sql.DB, parentID, childID int64, taskCallID string) {
	t.Helper()

	_, err := db.ExecContext(
		context.Background(),
		`INSERT INTO subagent_links (parent_id, child_id, task_call_id, blocking, depth, state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		parentID, childID, taskCallID, false, 0, "spawned", time.Now().UTC().Unix(),
	)
	require.NoError(t, err)
}

func TestStore_CreateSubagentSession_PersistsRootAndModel(t *testing.T) {
	s, _, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := s.CreateSession(ctx, projectID, "parent-model", "", nil)
	require.NoError(t, err)

	childID, err := createChild(ctx, s, projectID, parent.ID, parent.ID, "general", "child-model", "high")
	require.NoError(t, err)
	assert.Positive(t, childID)

	rec, err := s.GetSession(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, parent.ID, rec.ParentID)
	assert.Equal(t, parent.ID, rec.RootID)
	assert.Equal(t, "general", rec.AgentType)
	assert.Equal(t, "child-model", rec.Model)
	assert.Equal(t, "high", rec.ReasoningLevel)
}

func TestSubagentStore_CreateCommitsAggregate(t *testing.T) {
	s, db, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := s.CreateSession(ctx, projectID, "parent-model", "", nil)
	require.NoError(t, err)

	childID, err := newTestSubagentStore(db).Create(ctx, subagent.Create{
		ProjectID:      projectID,
		ParentID:       parent.ID,
		RootID:         parent.ID,
		AgentType:      "general",
		Model:          "child-model",
		ReasoningLevel: "high",
		TaskCallID:     "task-1",
		Blocking:       true,
		Depth:          1,
		State:          "spawned",
		InitialInput:   "inspect the repository",
	})
	require.NoError(t, err)

	rec, err := s.GetSession(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, parent.ID, rec.ParentID)
	assert.Equal(t, parent.ID, rec.RootID)
	assert.Equal(t, "child-model", rec.Model)

	var taskCallID, state string
	var blocking bool
	require.NoError(t, db.QueryRowContext(
		ctx,
		`SELECT task_call_id, blocking, state FROM subagent_links WHERE child_id = ?`,
		childID,
	).Scan(&taskCallID, &blocking, &state))
	assert.Equal(t, "task-1", taskCallID)
	assert.True(t, blocking)
	assert.Equal(t, "spawned", state)

	input, err := s.PeekPending(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.InputSourceAgent, input.Source)
	assert.Equal(t, "inspect the repository", input.RawContent)
}

func TestSubagentStore_CreateRejectsStoppingParent(t *testing.T) {
	s, db, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := s.CreateSession(ctx, projectID, "parent-model", "", nil)
	require.NoError(t, err)
	require.NoError(t, s.UpdateSessionStatus(ctx, parent.ID, sessionstore.SessionStatusStopping))

	_, err = newTestSubagentStore(db).Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		Model: "child-model", TaskCallID: "task-1", State: "spawned",
	})
	require.Error(t, err)

	var children int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE parent_id = ?`, parent.ID,
	).Scan(&children))
	assert.Zero(t, children)
}

func TestSubagentStore_CreateRollsBackAggregateOnInboxFailure(t *testing.T) {
	s, db, projectID := newTestStore(t)
	ctx := context.Background()
	parent, err := s.CreateSession(ctx, projectID, "parent-model", "", nil)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		CREATE TRIGGER reject_subagent_input
		BEFORE INSERT ON session_inbox
		BEGIN
			SELECT RAISE(FAIL, 'injected inbox failure');
		END;
	`)
	require.NoError(t, err)

	_, err = newTestSubagentStore(db).Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		Model: "child-model", TaskCallID: "task-1", State: "spawned",
		InitialInput: "work",
	})
	require.ErrorContains(t, err, "injected inbox failure")

	var children, links int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE parent_id = ?`, parent.ID,
	).Scan(&children))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM subagent_links`).Scan(&links))
	assert.Zero(t, children)
	assert.Zero(t, links)
}

func TestSubagentStore_CreateRollsBackOrphanOnLinkFailure(t *testing.T) {
	s, db, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := s.CreateSession(ctx, projectID, "parent-model", "", nil)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		CREATE TRIGGER reject_subagent_link
		BEFORE INSERT ON subagent_links
		BEGIN
			SELECT RAISE(FAIL, 'injected link failure');
		END;
	`)
	require.NoError(t, err)

	_, err = newTestSubagentStore(db).Create(ctx, subagent.Create{
		ProjectID:  projectID,
		ParentID:   parent.ID,
		RootID:     parent.ID,
		AgentType:  "general",
		Model:      "child-model",
		TaskCallID: "task-1",
		Depth:      1,
		State:      "spawned",
	})
	require.ErrorContains(t, err, "insert subagent link")

	var children, links int
	require.NoError(t, db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM sessions WHERE parent_id = ?`,
		parent.ID,
	).Scan(&children))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM subagent_links`).Scan(&links))
	assert.Zero(t, children, "a link failure must roll the child row back")
	assert.Zero(t, links)
}

func TestStore_GetChildSessionStats_ByRoot(t *testing.T) {
	s, _, projectID := newTestStore(t)
	ctx := context.Background()

	root, err := s.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	c1, _ := createChild(ctx, s, projectID, root.ID, root.ID, "general", "m", "")
	c2, _ := createChild(ctx, s, projectID, c1, root.ID, "general", "m", "")
	require.NoError(t, stepState(ctx, s, c1, 3, sessionstore.SessionStatusCompleted))
	require.NoError(t, stepState(ctx, s, c2, 4, sessionstore.SessionStatusCompleted))

	// A child of an unrelated root must not be counted.
	other, err := s.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	oc, _ := createChild(ctx, s, projectID, other.ID, other.ID, "general", "m", "")
	require.NoError(t, stepState(ctx, s, oc, 99, sessionstore.SessionStatusCompleted))

	count, iters, err := s.GetChildSessionStats(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.Equal(t, 7, iters)
}
