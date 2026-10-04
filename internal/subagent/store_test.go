package subagent

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func TestDeliverCompletionToStoppingParentSettlesLink(t *testing.T) {
	t.Parallel()
	links, sessions, db, projectID := newTestStore(t)
	ctx := context.Background()
	parent, err := sessions.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := links.Create(ctx, Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID, Model: "m",
		TaskCallID: "blocking", Blocking: true, State: StateRunning,
	})
	require.NoError(t, err)
	link, err := links.Finalize(ctx, childID, false)
	require.NoError(t, err)
	require.NotNil(t, link)
	require.NoError(t, sessions.UpdateSessionStatus(ctx, parent.ID, sessionstore.SessionStatusStopping))

	won, err := links.DeliverCompletion(ctx, *link, "result")
	require.NoError(t, err)
	assert.False(t, won)

	var deliveredAt, inputID sql.NullInt64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT delivered_at, delivered_input_id
		FROM subagent_links WHERE child_id = ?`, childID).Scan(&deliveredAt, &inputID))
	assert.True(t, deliveredAt.Valid)
	assert.False(t, inputID.Valid)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_inbox
		WHERE session_id = ? AND source = 'call_result'`, parent.ID).Scan(&count))
	assert.Zero(t, count)
}

func TestLinkStore_CreateAndRead(t *testing.T) {
	ls, ss, _, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := ls.Create(ctx, Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID, Model: "m",
		AgentType: "general", TaskCallID: "call-abc", Blocking: true, Depth: 1,
		State: StateSpawned,
	})
	require.NoError(t, err)

	got, err := ls.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, parent.ID, got.ParentID)
	assert.Equal(t, childID, got.ChildID)
	assert.Equal(t, "call-abc", got.TaskCallID)
	assert.True(t, got.Blocking)
	assert.Equal(t, 1, got.Depth)
	assert.Equal(t, StateSpawned, got.State)
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

func TestLinkStore_DeliverBackgroundCompletionToInbox(t *testing.T) {
	links, ss, _, projectID := newTestStore(t)
	ctx := context.Background()
	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := links.Create(ctx, Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		AgentType: "general", Model: "m", TaskCallID: "fixture", State: StateSpawned,
	})
	require.NoError(t, err)
	link := Link{ParentID: parent.ID, ChildID: childID, TaskCallID: "call", Depth: 1}
	require.NoError(t, seedChildLink(ctx, ss, link))
	require.NoError(t, seedTerminalChild(ctx, ss, childID, StateCompleted, "done", OutcomeCompleted))
	storedLink, err := links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, storedLink)
	won, err := links.DeliverBackgroundCompletion(ctx, *storedLink, 3)
	require.NoError(t, err)
	require.True(t, won)
	updated, err := links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.Positive(t, updated.DeliveredInputID)
	assert.Zero(t, updated.DeliveredMsgID)

	input, err := ss.PeekPending(ctx, parent.ID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.InputSourceSubagent, input.Source)
	assert.EqualValues(t, childID, input.Attributes["child_id"])
	assert.EqualValues(t, 1, input.Attributes["activation_seq"])
	assert.Equal(t, `<subagent_completion>
child_id: `+strconv.FormatInt(childID, 10)+`
activation_seq: 1
outcome: completed
iterations: 3
result:
done
</subagent_completion>`, input.RawContent)

	won, err = links.DeliverBackgroundCompletion(ctx, *storedLink, 3)
	require.NoError(t, err)
	assert.False(t, won)
	second, err := ss.PeekPending(ctx, parent.ID)
	require.NoError(t, err)
	assert.Equal(t, input.ID, second.ID)
}

func TestLinkStore_KilledParentRecoversBackgroundCompletionForSuppression(t *testing.T) {
	links, ss, _, projectID := newTestStore(t)
	ctx := context.Background()
	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	createTerminal := func(callID string, blocking bool) int64 {
		t.Helper()

		childID, createErr := links.Create(ctx, Create{
			ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
			AgentType: "general", Model: "m", TaskCallID: "fixture", State: StateSpawned,
		})
		require.NoError(t, createErr)
		require.NoError(t, seedChildLink(ctx, ss, Link{
			ParentID: parent.ID, ChildID: childID, TaskCallID: callID, Blocking: blocking,
		}))
		require.NoError(
			t,
			seedTerminalChild(ctx, ss, childID, StateCompleted, "done", OutcomeCompleted),
		)

		return childID
	}

	backgroundID := createTerminal("background", false)
	_ = createTerminal("blocking", true)
	require.NoError(
		t,
		ss.WithTx(ctx, func(tx *sql.Tx) error { return sessionstore.MarkSessionKilledTx(ctx, tx, parent.ID) }),
	)

	undelivered, err := links.ListUndeliveredParentLinks(ctx)
	require.NoError(t, err)
	require.Len(t, undelivered, 1)
	assert.Equal(t, backgroundID, undelivered[0].ChildID)

	won, err := links.DeliverBackgroundCompletion(ctx, undelivered[0], 1)
	require.NoError(t, err)
	require.False(t, won)
	updated, err := links.GetLink(ctx, backgroundID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Positive(t, updated.DeliveredAt)
	assert.Zero(t, updated.DeliveredInputID)
	_, err = ss.PeekPending(ctx, parent.ID)
	require.ErrorIs(t, err, sessionstore.ErrNoPendingInput)
}

func TestLinkStore_BackgroundCompletionRejectsStaleIdentity(t *testing.T) {
	links, ss, _, projectID := newTestStore(t)
	ctx := context.Background()
	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	otherParent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := links.Create(ctx, Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		AgentType: "general", Model: "m", TaskCallID: "fixture", State: StateSpawned,
	})
	require.NoError(t, err)
	require.NoError(t, seedChildLink(ctx, ss, Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "call", Depth: 1,
	}))
	require.NoError(t, seedTerminalChild(ctx, ss, childID, StateCompleted, "done", OutcomeCompleted))
	link, err := links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)

	wrongParent := *link
	wrongParent.ParentID = otherParent.ID
	_, err = links.DeliverBackgroundCompletion(ctx, wrongParent, 1)
	require.Error(t, err)
	stale := *link
	stale.ActivationSeq++
	applied, err := links.DeliverBackgroundCompletion(ctx, stale, 1)
	require.NoError(t, err)
	require.False(t, applied)

	_, err = ss.PeekPending(ctx, parent.ID)
	require.ErrorIs(t, err, sessionstore.ErrNoPendingInput)
	_, err = ss.PeekPending(ctx, otherParent.ID)
	require.ErrorIs(t, err, sessionstore.ErrNoPendingInput)
}

func TestLinkStore_Finalize(t *testing.T) {
	links, ss, _, projectID := newTestStore(t)
	ctx := t.Context()
	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := links.Create(ctx, Create{
		ProjectID: projectID, ParentID: parent.ID,
		RootID: parent.ID, Model: "m", TaskCallID: "c1", State: StateRunning,
	})
	require.NoError(t, err)
	_, err = ss.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: childID,
			Messages:  []*transcript.Message{{Role: "assistant", Content: "the answer is 42"}},
		},
	)
	require.NoError(t, err)
	link, err := links.Finalize(ctx, childID, false)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, StateCompleted, link.State)
	assert.True(t, link.Terminal())
	assert.Equal(t, "the answer is 42", link.Result)
	assert.Equal(t, OutcomeCompleted, link.Outcome)
	rec, err := ss.GetSession(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusCompleted, rec.Status)
	duplicate, err := links.Finalize(ctx, childID, true)
	require.NoError(t, err)
	assert.Nil(t, duplicate)
	preserved, err := links.GetLink(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, link.Result, preserved.Result)
}

func TestLinkStore_ResetRunning(t *testing.T) {
	ls, ss, _, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := ls.Create(ctx, Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		AgentType: "general", Model: "m", TaskCallID: "fixture", State: StateSpawned,
	})
	require.NoError(t, err)
	require.NoError(
		t,
		seedChildLink(ctx, ss, Link{ParentID: parent.ID, ChildID: childID, TaskCallID: "c1"}),
	)

	require.NoError(
		t,
		seedTerminalChild(ctx, ss, childID, StateCompleted, "answer", OutcomeCompleted),
	)
	deliverOneLink(t, ls, parent.ID, childID)

	require.NoError(t, ls.Resume(ctx, childID))

	link, err := ls.GetLink(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, link.State)
	assert.Zero(t, link.DeliveredAt)
	assert.Zero(t, link.DeliveredMsgID)
	// result/outcome are intentionally left stale until the next terminalization.
	assert.Equal(t, "answer", link.Result)
}

func TestLinkStoreRejectsMissingResumeAndKill(t *testing.T) {
	links, _, _, _ := newTestStore(t)
	require.ErrorContains(t, links.Resume(t.Context(), 999), "not found")
	require.ErrorContains(t, links.Kill(t.Context(), 999), "not found")
}

func TestLinkStore_ListPending(t *testing.T) {
	ls, ss, _, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	c1, _ := ls.Create(ctx, Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		AgentType: "general", Model: "m", TaskCallID: "fixture", State: StateSpawned,
	})
	c2, _ := ls.Create(ctx, Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		AgentType: "general", Model: "m", TaskCallID: "fixture", State: StateSpawned,
	})
	require.NoError(t, seedChildLink(ctx, ss, Link{ParentID: parent.ID, ChildID: c1, TaskCallID: "c1"}))
	require.NoError(t, seedChildLink(ctx, ss, Link{ParentID: parent.ID, ChildID: c2, TaskCallID: "c2"}))

	pending, err := ls.ListPendingChildLinks(ctx, parent.ID)
	require.NoError(t, err)
	assert.Len(t, pending, 2)

	deliverOneLink(t, ls, parent.ID, c1)

	pending, err = ls.ListPendingChildLinks(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, c2, pending[0].ChildID)
}

func TestLinkStore_ListRunningAndUndelivered(t *testing.T) {
	ls, ss, _, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := ss.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	running, _ := ls.Create(ctx, Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		AgentType: "general", Model: "m", TaskCallID: "fixture", State: StateSpawned,
	})
	done, _ := ls.Create(ctx, Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		AgentType: "general", Model: "m", TaskCallID: "fixture", State: StateSpawned,
	})
	require.NoError(t, seedChildLink(ctx, ss, Link{
		ParentID: parent.ID, ChildID: running, TaskCallID: "r",
	}))
	require.NoError(t, seedChildLink(ctx, ss, Link{ParentID: parent.ID, ChildID: done, TaskCallID: "d"}))

	require.NoError(
		t,
		seedTerminalChild(ctx, ss, done, StateCompleted, "the result", OutcomeCompleted),
	)

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
	assert.Equal(t, OutcomeCompleted, undelivered[0].Outcome)

	// Once delivered, it drops out of the undelivered set.
	deliverOneLink(t, ls, parent.ID, done)
	undelivered, err = ls.ListUndeliveredParentLinks(ctx)
	require.NoError(t, err)
	assert.Empty(t, undelivered)
}

func TestSubagentStore_CreateCommitsAggregate(t *testing.T) {
	links, s, db, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := s.CreateSession(ctx, projectID, "parent-model", "", nil)
	require.NoError(t, err)

	childID, err := links.Create(ctx, Create{
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
	links, s, db, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := s.CreateSession(ctx, projectID, "parent-model", "", nil)
	require.NoError(t, err)
	require.NoError(t, s.UpdateSessionStatus(ctx, parent.ID, sessionstore.SessionStatusStopping))

	_, err = links.Create(ctx, Create{
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
	links, s, db, projectID := newTestStore(t)
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

	_, err = links.Create(ctx, Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		Model: "child-model", TaskCallID: "task-1", State: "spawned",
		InitialInput: "work",
	})
	require.ErrorContains(t, err, "injected inbox failure")

	var children, linkCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE parent_id = ?`, parent.ID,
	).Scan(&children))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM subagent_links`).Scan(&linkCount))
	assert.Zero(t, children)
	assert.Zero(t, linkCount)
}

func TestSubagentStore_CreateRollsBackOrphanOnLinkFailure(t *testing.T) {
	links, s, db, projectID := newTestStore(t)
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

	_, err = links.Create(ctx, Create{
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

	var children, linkCount int
	require.NoError(t, db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM sessions WHERE parent_id = ?`,
		parent.ID,
	).Scan(&children))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM subagent_links`).Scan(&linkCount))
	assert.Zero(t, children, "a link failure must roll the child row back")
	assert.Zero(t, linkCount)
}

func TestStoreTransitionsAreAtomic(t *testing.T) {
	tests := []struct {
		name        string
		state       State
		status      sessionstore.SessionStatus
		outcome     Outcome
		transition  func(*testing.T, context.Context, *store, int64) error
		wantState   State
		wantStatus  sessionstore.SessionStatus
		wantOutcome Outcome
	}{
		{
			name: "Finalize", state: StateRunning, status: sessionstore.SessionStatusActive,
			transition: func(_ *testing.T, ctx context.Context, links *store, id int64) error {
				_, err := links.Finalize(ctx, id, false)
				return err
			},
			wantState: StateCompleted, wantStatus: sessionstore.SessionStatusCompleted,
			wantOutcome: OutcomeCompleted,
		},
		{
			name: "Kill", state: StateRunning, status: sessionstore.SessionStatusActive,
			transition: func(_ *testing.T, ctx context.Context, links *store, id int64) error {
				return links.Kill(ctx, id)
			},
			wantState: StateKilled, wantStatus: sessionstore.SessionStatusKilled, wantOutcome: OutcomeKilled,
		},
		{
			name: "Resume", state: StateCompleted, status: sessionstore.SessionStatusCompleted,
			outcome: OutcomeCompleted,
			transition: func(_ *testing.T, ctx context.Context, links *store, id int64) error {
				return links.Resume(ctx, id)
			},
			wantState: StateRunning, wantStatus: sessionstore.SessionStatusActive, wantOutcome: OutcomeCompleted,
		},
		{
			name: "Rearm", state: StateCompleted, status: sessionstore.SessionStatusCompleted,
			outcome: OutcomeCompleted,
			transition: func(t *testing.T, ctx context.Context, links *store, id int64) error {
				rearmed, err := links.Rearm(ctx, id)
				if err == nil {
					require.True(t, rearmed)
				}
				return err
			},
			wantState: StateRunning, wantStatus: sessionstore.SessionStatusActive, wantOutcome: OutcomeCompleted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			links, sessions, db, projectID := newTestStore(t)
			ctx := t.Context()
			parent, err := sessions.CreateSession(ctx, projectID, "m", "", nil)
			require.NoError(t, err)
			childID, err := links.Create(ctx, Create{
				ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
				Model: "m", TaskCallID: "atomic", State: tt.state, Blocking: true,
			})
			require.NoError(t, err)
			require.NoError(t, sessions.UpdateSessionStatus(ctx, childID, tt.status))
			_, err = db.ExecContext(ctx, `UPDATE subagent_links SET outcome=?,result=? WHERE child_id=?`,
				tt.outcome, "previous", childID)
			require.NoError(t, err)
			if tt.name == "Finalize" {
				_, err = sessions.Commit(ctx, sessionstore.Commit{
					SessionID: childID,
					Messages:  []*transcript.Message{{Role: "assistant", Content: "answer"}},
				})
				require.NoError(t, err)
			}
			if tt.name == "Rearm" {
				_, err = db.ExecContext(ctx, `UPDATE subagent_links SET delivered_at=1 WHERE child_id=?`, childID)
				require.NoError(t, err)
				_, err = sessions.Enqueue(ctx, sessionstore.Input{
					SessionID: childID, Source: sessionstore.InputSourceAgent, Content: "follow-up",
				})
				require.NoError(t, err)
			}
			before, err := links.GetLink(ctx, childID)
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `CREATE TEMP TRIGGER reject_child_status
				BEFORE UPDATE OF status ON sessions BEGIN SELECT RAISE(ABORT, 'injected'); END`)
			require.NoError(t, err)
			require.ErrorContains(t, tt.transition(t, ctx, links, childID), "injected")
			after, err := links.GetLink(ctx, childID)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			record, err := sessions.GetSession(ctx, childID)
			require.NoError(t, err)
			assert.Equal(t, tt.status, record.Status)
			assert.Nil(t, record.KilledAt)
			_, err = db.ExecContext(ctx, `DROP TRIGGER reject_child_status`)
			require.NoError(t, err)
			require.NoError(t, tt.transition(t, ctx, links, childID))
			after, err = links.GetLink(ctx, childID)
			require.NoError(t, err)
			assert.Equal(t, tt.wantState, after.State)
			assert.Equal(t, tt.wantOutcome, after.Outcome)
			record, err = sessions.GetSession(ctx, childID)
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, record.Status)
		})
	}
}

func newTestStore(t *testing.T) (*store, *sessionstore.Store, *sql.DB, int64) {
	t.Helper()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, migrate.Run(ctx, db, dbPath))
	result, err := db.ExecContext(ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test")
	require.NoError(t, err)
	projectID, err := result.LastInsertId()
	require.NoError(t, err)
	sessions := sessionstore.NewStore(db)
	return &store{db: db, sessions: sessions}, sessions, db, projectID
}

func seedChildLink(ctx context.Context, sessions *sessionstore.Store, link Link) error {
	if link.State == "" {
		link.State = StateSpawned
	}
	return sessions.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(
			ctx,
			`UPDATE subagent_links SET task_call_id=?,blocking=?,depth=?,state=?,result=?,outcome=? WHERE child_id=?`,
			link.TaskCallID,
			link.Blocking,
			link.Depth,
			link.State,
			link.Result,
			link.Outcome,
			link.ChildID,
		)
		return err
	})
}

func seedTerminalChild(ctx context.Context, sessions *sessionstore.Store, childID int64,
	state State, result string, outcome Outcome,
) error {
	return sessions.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE subagent_links SET state=?,result=?,outcome=? WHERE child_id=?`,
			state, result, outcome, childID)
		return err
	})
}

func deliverOneLink(t *testing.T, links Store, parentID, childID int64) {
	t.Helper()
	link, err := links.GetLink(context.Background(), childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	require.Equal(t, parentID, link.ParentID)
	if !link.Terminal() {
		link, err = links.Finalize(context.Background(), childID, false)
		require.NoError(t, err)
		require.NotNil(t, link)
	}
	won, err := links.DeliverBackgroundCompletion(context.Background(), *link, 1)
	require.NoError(t, err)
	require.True(t, won)
}
