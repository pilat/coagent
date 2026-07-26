package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
)

// newTestLinkStore opens a migrated temp SQLite DB and returns a sessionstore.Store
// (for the session/message rows link tests reference), a LinkStore, and a project
// id the sessions can reference (FKs are enforced).
func newTestLinkStore(t *testing.T) (sessionstore.Store, LinkStore, int64) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))

	res, err := db.ExecContext(
		context.Background(),
		`INSERT INTO projects (work_dir, name) VALUES (?, ?)`,
		t.TempDir(), "test",
	)
	require.NoError(t, err)
	projectID, err := res.LastInsertId()
	require.NoError(t, err)

	return sessionstore.NewStore(db), NewLinkStore(db), projectID
}

// deliverOneLink wins the delivery CAS for childID via the session store (the sole
// writer of delivered_at), inserting one completion message into parentID's
// transcript — the test stand-in for a delivered completion.
func deliverOneLink(t *testing.T, ss sessionstore.Store, parentID, childID int64) []int64 {
	t.Helper()

	ids, won, err := ss.DeliverCompletionAtomic(context.Background(), parentID, []*sessionstore.StoredMessage{{
		Role: llmwire.RoleTool, Content: "done", ToolCallID: "x", ToolName: "task",
	}}, childID, 1)
	require.NoError(t, err)
	require.True(t, won)

	return ids
}

func TestLinkStore_InsertAndRead(t *testing.T) {
	ss, ls, projectID := newTestLinkStore(t)
	ctx := context.Background()

	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := ss.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	require.NoError(t, err)

	link := SubagentLink{
		ParentID:   parent.ID,
		ChildID:    childID,
		TaskCallID: "call-abc",
		Blocking:   true,
		Depth:      1,
		TimeoutSec: 300,
	}
	require.NoError(t, ls.InsertSubagentLink(ctx, link))

	got, err := ls.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, parent.ID, got.ParentID)
	assert.Equal(t, childID, got.ChildID)
	assert.Equal(t, "call-abc", got.TaskCallID)
	assert.True(t, got.Blocking)
	assert.Equal(t, 1, got.Depth)
	assert.Equal(t, LinkStateSpawned, got.State)
	assert.Equal(t, 300, got.TimeoutSec)
	assert.Zero(t, got.DeliveredAt)
	assert.Positive(t, got.CreatedAt)

	byCall, err := ls.GetLinkByTaskCallID(ctx, parent.ID, "call-abc")
	require.NoError(t, err)
	require.NotNil(t, byCall)
	assert.Equal(t, childID, byCall.ChildID)

	missing, err := ls.GetLink(ctx, 999999)
	require.NoError(t, err)
	assert.Nil(t, missing)
}

func TestLinkStore_MarkTerminal(t *testing.T) {
	ss, ls, projectID := newTestLinkStore(t)
	ctx := context.Background()

	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := ss.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	require.NoError(t, err)
	require.NoError(t, ls.InsertSubagentLink(ctx, SubagentLink{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "c1",
	}))

	// Terminalization is now two calls: the link row (LinkStore) then the session
	// status (sessionstore.Store). Both effects are asserted below.
	require.NoError(t, ls.MarkLinkTerminal(
		ctx, childID, LinkStateCompleted, "the answer is 42", LinkOutcomeCompleted,
	))
	require.NoError(t, ss.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))

	link, err := ls.GetLink(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, LinkStateCompleted, link.State)
	assert.True(t, link.Terminal())
	assert.Equal(t, "the answer is 42", link.Result)
	assert.Equal(t, LinkOutcomeCompleted, link.Outcome)

	rec, err := ss.GetSession(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusCompleted, rec.Status)

	// A second terminalization (re-engagement) overwrites result/outcome
	// unconditionally — even to a different outcome.
	require.NoError(t, ls.MarkLinkTerminal(ctx, childID, LinkStateError, "", LinkOutcomeIncomplete))
	link, err = ls.GetLink(ctx, childID)
	require.NoError(t, err)
	assert.Empty(t, link.Result)
	assert.Equal(t, LinkOutcomeIncomplete, link.Outcome)
}

func TestLinkStore_ResetRunning(t *testing.T) {
	ss, ls, projectID := newTestLinkStore(t)
	ctx := context.Background()

	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := ss.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	require.NoError(t, err)
	require.NoError(
		t,
		ls.InsertSubagentLink(ctx, SubagentLink{ParentID: parent.ID, ChildID: childID, TaskCallID: "c1"}),
	)

	require.NoError(t, ls.MarkLinkTerminal(ctx, childID, LinkStateCompleted, "answer", LinkOutcomeCompleted))
	deliverOneLink(t, ss, parent.ID, childID)

	require.NoError(t, ls.ResetLinkRunning(ctx, childID))

	link, err := ls.GetLink(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, LinkStateRunning, link.State)
	assert.Zero(t, link.DeliveredAt)
	assert.Zero(t, link.DeliveredMsgID)
	// result/outcome are intentionally left stale until the next terminalization.
	assert.Equal(t, "answer", link.Result)
}

func TestLinkStoreRejectsMixedOrMissingTerminalUpdates(t *testing.T) {
	_, ls, _ := newTestLinkStore(t)
	ctx := context.Background()

	err := ls.MarkLinkTerminal(ctx, 999, LinkStateCompleted, "answer", LinkOutcomeError)
	require.ErrorContains(t, err, "invalid terminal link state/outcome")

	err = ls.MarkLinkTerminal(ctx, 999, LinkStateCompleted, "answer", LinkOutcomeCompleted)
	require.ErrorContains(t, err, "not found")

	err = ls.ResetLinkRunning(ctx, 999)
	require.Error(t, err)
	assert.ErrorContains(t, err, "not found")
}

func TestLinkStore_ListPending(t *testing.T) {
	ss, ls, projectID := newTestLinkStore(t)
	ctx := context.Background()

	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	c1, _ := ss.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	c2, _ := ss.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	require.NoError(t, ls.InsertSubagentLink(ctx, SubagentLink{ParentID: parent.ID, ChildID: c1, TaskCallID: "c1"}))
	require.NoError(t, ls.InsertSubagentLink(ctx, SubagentLink{ParentID: parent.ID, ChildID: c2, TaskCallID: "c2"}))

	pending, err := ls.ListPendingChildLinks(ctx, parent.ID)
	require.NoError(t, err)
	assert.Len(t, pending, 2)

	deliverOneLink(t, ss, parent.ID, c1)

	pending, err = ls.ListPendingChildLinks(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, c2, pending[0].ChildID)
}

func TestLinkStore_ListRunningAndUndelivered(t *testing.T) {
	ss, ls, projectID := newTestLinkStore(t)
	ctx := context.Background()

	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	running, _ := ss.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	done, _ := ss.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	require.NoError(t, ls.InsertSubagentLink(ctx, SubagentLink{ParentID: parent.ID, ChildID: running, TaskCallID: "r"}))
	require.NoError(t, ls.InsertSubagentLink(ctx, SubagentLink{ParentID: parent.ID, ChildID: done, TaskCallID: "d"}))

	require.NoError(t, ls.MarkLinkTerminal(ctx, done, LinkStateCompleted, "the result", LinkOutcomeCompleted))

	runningLinks, err := ls.ListRunningChildLinks(ctx)
	require.NoError(t, err)
	require.Len(t, runningLinks, 1)
	assert.Equal(t, running, runningLinks[0].ChildID)

	undelivered, err := ls.ListUndeliveredParentLinks(ctx)
	require.NoError(t, err)
	require.Len(t, undelivered, 1)
	assert.Equal(t, done, undelivered[0].ChildID)
	// The sl.-aliased join columns carry result/outcome through too.
	assert.Equal(t, "the result", undelivered[0].Result)
	assert.Equal(t, LinkOutcomeCompleted, undelivered[0].Outcome)

	// Once delivered, it drops out of the undelivered set.
	deliverOneLink(t, ss, parent.ID, done)
	undelivered, err = ls.ListUndeliveredParentLinks(ctx)
	require.NoError(t, err)
	assert.Empty(t, undelivered)
}
