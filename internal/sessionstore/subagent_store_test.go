package sessionstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/subagent"
)

func newTestSubagentTransactions(db *sql.DB) subagent.Transactions {
	return subagent.NewTransactions(db)
}

// newTestStore opens a migrated temp SQLite DB and returns a real store, its raw
// DB handle (for seeding subagent_links rows the session store no longer writes),
// and a project id the sessions can reference (FKs are enforced).
func newTestStore(t *testing.T) (Store, *sql.DB, int64) {
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

	return NewStore(db), db, projectID
}

// seedLink inserts a bare row for tests that exercise a specific ledger state.
func seedLink(t *testing.T, db *sql.DB, parentID, childID int64, taskCallID string) {
	t.Helper()

	_, err := db.ExecContext(
		context.Background(),
		`INSERT INTO subagent_links (parent_id, child_id, task_call_id, blocking, depth, state, timeout_sec, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		parentID, childID, taskCallID, false, 0, "spawned", 0, time.Now().UTC().Unix(),
	)
	require.NoError(t, err)
}

// readLinkDelivery reads the delivery markers DeliverCompletion stamps.
func readLinkDelivery(t *testing.T, db *sql.DB, childID int64) (int64, int64) {
	t.Helper()

	var deliveredAt, deliveredMsgID sql.NullInt64
	err := db.QueryRowContext(
		context.Background(),
		`SELECT delivered_at, delivered_msg_id FROM subagent_links WHERE child_id = ?`,
		childID,
	).Scan(&deliveredAt, &deliveredMsgID)
	require.NoError(t, err)

	return deliveredAt.Int64, deliveredMsgID.Int64
}

func TestStore_CreateSubagentSession_PersistsRootAndModel(t *testing.T) {
	s, _, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := s.CreateSession(ctx, projectID, "parent-model", "", nil)
	require.NoError(t, err)

	childID, err := s.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "child-model", "high")
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

	childID, err := newTestSubagentTransactions(db).Create(ctx, subagent.Create{
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
		TimeoutSec:     30,
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
	assert.Equal(t, InputSourceAgent, input.Source)
	assert.Equal(t, "inspect the repository", input.RawContent)
}

func TestSubagentStore_CreateRejectsStoppingParent(t *testing.T) {
	s, db, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := s.CreateSession(ctx, projectID, "parent-model", "", nil)
	require.NoError(t, err)
	require.NoError(t, s.UpdateSessionStatus(ctx, parent.ID, SessionStatusStopping))

	_, err = newTestSubagentTransactions(db).Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		Model: "child-model", TaskCallID: "task-1", State: "spawned",
	})
	require.ErrorContains(t, err, "not accepting subagents")

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

	_, err = newTestSubagentTransactions(db).Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		Model: "child-model", TaskCallID: "task-1", State: "spawned",
		InitialInput: "work",
	})
	require.ErrorContains(t, err, "insert subagent initial input")

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

	_, err = newTestSubagentTransactions(db).Create(ctx, subagent.Create{
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

func TestSubagentStore_DeliverCompletion_CAS(t *testing.T) {
	s, db, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := s.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := s.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	require.NoError(t, err)
	seedLink(t, db, parent.ID, childID, "c1")

	msg := func(c string) []*StoredMessage {
		return []*StoredMessage{{Role: llmwire.RoleTool, Content: c, ToolCallID: "c1", ToolName: "task"}}
	}

	ids, won, err := newTestSubagentTransactions(db).DeliverCompletion(ctx, parent.ID, msg("first"), childID, 1)
	require.NoError(t, err)
	assert.True(t, won, "first delivery wins the CAS")
	require.Len(t, ids, 1)

	ids2, won2, err := newTestSubagentTransactions(db).DeliverCompletion(ctx, parent.ID, msg("second"), childID, 1)
	require.NoError(t, err)
	assert.False(t, won2, "second delivery loses the CAS")
	assert.Empty(t, ids2, "the loser inserts nothing")

	deliveredAt, deliveredMsgID := readLinkDelivery(t, db, childID)
	assert.Positive(t, deliveredAt)
	assert.Equal(t, ids[0], deliveredMsgID, "delivered_msg_id is the winner's last insert")

	// Only the winning delivery's message lands in the parent transcript — the
	// CAS-loser's insert never ran (row count unchanged).
	msgs, err := s.LoadActiveMessages(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "first", msgs[0].Content)
}

func TestStore_StaleCompletionCannotCrossSubagentRearmBoundary(t *testing.T) {
	s, db, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := s.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := s.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	require.NoError(t, err)
	seedLink(t, db, parent.ID, childID, "c1")

	completion := func(content string) []*StoredMessage {
		return []*StoredMessage{{
			Role: llmwire.RoleTool, Content: content, ToolCallID: "c1", ToolName: "task",
		}}
	}

	_, won, err := newTestSubagentTransactions(db).DeliverCompletion(
		ctx, parent.ID, completion("activation one"), childID, 1,
	)
	require.NoError(t, err)
	require.True(t, won)
	_, err = db.ExecContext(ctx, `UPDATE subagent_links SET state = 'completed' WHERE child_id = ?`, childID)
	require.NoError(t, err)
	_, err = s.EnqueueInput(ctx, childID, InputSourceAgent, "follow-up")
	require.NoError(t, err)

	rearmed, err := newTestSubagentTransactions(db).RearmDeliveredWithPendingInput(ctx, childID)
	require.NoError(t, err)
	require.True(t, rearmed)

	_, staleWon, err := newTestSubagentTransactions(db).DeliverCompletion(
		ctx, parent.ID, completion("stale duplicate"), childID, 1,
	)
	require.NoError(t, err)
	assert.False(t, staleWon, "activation one cannot deliver after activation two has begun")

	_, currentWon, err := newTestSubagentTransactions(db).DeliverCompletion(
		ctx, parent.ID, completion("activation two"), childID, 2,
	)
	require.NoError(t, err)
	assert.True(t, currentWon)

	messages, err := s.LoadActiveMessages(ctx, parent.ID)
	require.NoError(t, err)
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	assert.Equal(t, []string{"activation one", "activation two"}, contents)
}

func TestSubagentStore_DeliverCompletionRejectsWrongParent(t *testing.T) {
	s, db, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := s.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	otherParent, err := s.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := s.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	require.NoError(t, err)
	seedLink(t, db, parent.ID, childID, "c1")

	_, won, err := newTestSubagentTransactions(db).DeliverCompletion(ctx, otherParent.ID, []*StoredMessage{{
		Role: llmwire.RoleTool, Content: "wrong parent", ToolCallID: "c1", ToolName: "task",
	}}, childID, 1)
	require.Error(t, err)
	assert.False(t, won)
	require.ErrorContains(t, err, "belongs to parent")

	deliveredAt, deliveredMsgID := readLinkDelivery(t, db, childID)
	assert.Zero(t, deliveredAt, "a rejected delivery must leave the CAS available for the real parent")
	assert.Zero(t, deliveredMsgID)

	messages, err := s.LoadActiveMessages(ctx, otherParent.ID)
	require.NoError(t, err)
	assert.Empty(t, messages, "a rejected delivery must not contaminate another transcript")
}

func TestSubagentStore_DeliverCompletionRejectsEmptyCompletion(t *testing.T) {
	s, db, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := s.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := s.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	require.NoError(t, err)
	seedLink(t, db, parent.ID, childID, "c1")

	_, won, err := newTestSubagentTransactions(db).DeliverCompletion(ctx, parent.ID, nil, childID, 1)
	require.Error(t, err)
	assert.False(t, won)
	require.ErrorContains(t, err, "no messages")

	deliveredAt, deliveredMsgID := readLinkDelivery(t, db, childID)
	assert.Zero(t, deliveredAt, "an empty completion must remain retryable")
	assert.Zero(t, deliveredMsgID)
}

func TestStore_InsertToolNotificationPairOnce(t *testing.T) {
	s, _, projectID := newTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	assistant := &StoredMessage{
		Role:      llmwire.RoleAssistant,
		ToolCalls: []byte(`[{"ID":"call-1","Name":"subagent_event"}]`),
	}
	toolResult := &StoredMessage{
		Role:       llmwire.RoleTool,
		ToolCallID: "call-1",
		ToolName:   "subagent_event",
		Content:    "child 5 done",
	}

	asstID, resultID, inserted, err := s.InsertToolNotificationPairOnce(
		ctx, sess.ID, "d1", "fp1", assistant, toolResult,
	)
	require.NoError(t, err)
	require.True(t, inserted)
	assert.Positive(t, asstID)
	assert.Positive(t, resultID)

	msgs, err := s.LoadActiveMessages(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, llmwire.RoleAssistant, msgs[0].Role)
	assert.Equal(t, llmwire.RoleTool, msgs[1].Role)
	assert.Equal(t, "call-1", msgs[1].ToolCallID)
}

func TestStore_ReplaceCompactedMessagesPreservesReplacementOrder(t *testing.T) {
	s, _, projectID := newTestStore(t)
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	headerID, err := s.InsertMessage(ctx, sess.ID, &StoredMessage{Role: llmwire.RoleUser, Content: "task"})
	require.NoError(t, err)
	oldID, err := s.InsertMessage(ctx, sess.ID, &StoredMessage{Role: llmwire.RoleAssistant, Content: "old"})
	require.NoError(t, err)
	retainedID, err := s.InsertMessage(ctx, sess.ID, &StoredMessage{Role: llmwire.RoleAssistant, Content: "retained"})
	require.NoError(t, err)
	_, err = s.InsertMessage(ctx, sess.ID, &StoredMessage{Role: llmwire.RoleUser, Content: "concurrent"})
	require.NoError(t, err)

	ids, err := s.ReplaceCompactedMessages(ctx, sess.ID, []int64{oldID}, []CompactionEntry{
		{ExistingID: headerID},
		{Message: &StoredMessage{Role: llmwire.RoleUser, Content: "summary"}},
		{Message: &StoredMessage{Role: llmwire.RoleAssistant, Content: "ack"}},
		{Message: &StoredMessage{Role: llmwire.RoleUser, Content: "skill"}},
		{ExistingID: retainedID},
	})
	require.NoError(t, err)
	require.Len(t, ids, 5)
	assert.Equal(t, headerID, ids[0])
	assert.Equal(t, retainedID, ids[4])

	messages, err := s.LoadActiveMessages(ctx, sess.ID)
	require.NoError(t, err)
	contents := make([]string, len(messages))
	for i, message := range messages {
		contents[i] = message.Content
	}

	assert.Equal(t, []string{"task", "summary", "ack", "skill", "retained", "concurrent"}, contents)
}

func TestStore_GetChildSessionStats_ByRoot(t *testing.T) {
	s, _, projectID := newTestStore(t)
	ctx := context.Background()

	root, err := s.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	c1, _ := s.CreateSubagentSession(ctx, projectID, root.ID, root.ID, "general", "m", "")
	c2, _ := s.CreateSubagentSession(ctx, projectID, c1, root.ID, "general", "m", "")
	require.NoError(t, s.UpdateSessionIteration(ctx, c1, 3, SessionStatusCompleted))
	require.NoError(t, s.UpdateSessionIteration(ctx, c2, 4, SessionStatusCompleted))

	// A child of an unrelated root must not be counted.
	other, err := s.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	oc, _ := s.CreateSubagentSession(ctx, projectID, other.ID, other.ID, "general", "m", "")
	require.NoError(t, s.UpdateSessionIteration(ctx, oc, 99, SessionStatusCompleted))

	count, iters, err := s.GetChildSessionStats(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.Equal(t, 7, iters)
}
