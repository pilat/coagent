package subagent_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/transcript"
)

func TestCompletionInvalidationFailureRollsBackDelivery(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	project, err := db.ExecContext(ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test")
	require.NoError(t, err)
	projectID, err := project.LastInsertId()
	require.NoError(t, err)
	store := sessionstore.NewStore(db)
	parent, err := store.CreateSession(ctx, projectID, "model", "medium", nil)
	require.NoError(t, err)
	candidateID, err := store.InsertMessage(ctx, parent.ID, &transcript.Message{
		Role: "assistant", Content: "candidate", FinishType: "stop",
	})
	require.NoError(t, err)
	_, err = db.ExecContext(
		ctx,
		`UPDATE sessions SET completion_check_candidate_id = ? WHERE id = ?`,
		candidateID,
		parent.ID,
	)
	require.NoError(t, err)

	transactions, err := subagent.NewTransactions(db, sessionstore.InvalidateCompletionCheckTx)
	require.NoError(t, err)
	childID, err := transactions.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		AgentType: "general", Model: "model", TaskCallID: "task-1", Blocking: true,
		Depth: 1, State: subagent.StateRunning,
	})
	require.NoError(t, err)
	links := subagent.NewStore(db)
	require.NoError(
		t,
		links.MarkLinkTerminal(ctx, childID, subagent.StateCompleted, "child answer", subagent.OutcomeCompleted),
	)
	link, err := links.GetLink(ctx, childID)
	require.NoError(t, err)

	invalidationFailure := errors.New("invalidation interrupted")
	failing, err := subagent.NewTransactions(
		db,
		func(ctx context.Context, tx *sql.Tx, sessionID int64, now time.Time) error {
			if err := sessionstore.InvalidateCompletionCheckTx(ctx, tx, sessionID, now); err != nil {
				return err
			}

			return invalidationFailure
		},
	)
	require.NoError(t, err)
	messages := []*transcript.Message{{Role: "tool", ToolName: "task", ToolCallID: "task-1", Content: "child answer"}}
	_, won, err := failing.DeliverCompletion(ctx, parent.ID, messages, childID, link.ActivationSeq)
	require.ErrorIs(t, err, invalidationFailure)
	assert.False(t, won)

	state, err := store.LoadCompletionCheckState(ctx, parent.ID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID)
	assert.Equal(t, candidateID, *state.CandidateID)
	link, err = links.GetLink(ctx, childID)
	require.NoError(t, err)
	assert.Zero(t, link.DeliveredAt)
	history, err := store.LoadActiveMessages(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, history, 1)

	ids, won, err := transactions.DeliverCompletion(ctx, parent.ID, messages, childID, link.ActivationSeq)
	require.NoError(t, err)
	require.True(t, won)
	require.Len(t, ids, 1)
	state, err = store.LoadCompletionCheckState(ctx, parent.ID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID)
	_, won, err = transactions.DeliverCompletion(ctx, parent.ID, messages, childID, link.ActivationSeq)
	require.NoError(t, err)
	assert.False(t, won)
	history, err = store.LoadActiveMessages(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, history, 2)
	assert.Equal(t, "child answer", history[1].Content)
}

func TestTransactionsRequireCompletionInvalidation(t *testing.T) {
	transactions, err := subagent.NewTransactions(nil, nil)
	require.ErrorContains(t, err, "completion invalidator is required")
	assert.Nil(t, transactions)
}
