package sessionstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

func TestPromotionAdvancesGeneration(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "hello")
	require.NoError(t, err)

	var gen int64
	var boundary sql.NullInt64
	loadState := func() {
		require.NoError(t, db.QueryRow(`SELECT model_input_generation, model_input_boundary
			FROM sessions WHERE id = ?`, session.ID).Scan(&gen, &boundary))
	}
	loadState()
	assert.Equal(t, int64(0), gen)
	assert.False(t, boundary.Valid)

	msg, err := store.PromoteInput(ctx, input.ID, "hello")
	require.NoError(t, err)

	loadState()
	assert.Equal(t, int64(1), gen)
	require.NotNil(t, boundary)
	assert.Equal(t, msg.ID, boundary.Int64)

	// A second promoted input advances again; each boundary is its own message.
	input2, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "again")
	require.NoError(t, err)
	msg2, err := store.PromoteInput(ctx, input2.ID, "again")
	require.NoError(t, err)
	loadState()
	assert.Equal(t, int64(2), gen)
	assert.Equal(t, msg2.ID, boundary.Int64)
}

func TestPromotionIdempotentReplayDoesNotAdvanceGeneration(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "hello")
	require.NoError(t, err)
	_, err = store.PromoteInput(ctx, input.ID, "hello")
	require.NoError(t, err)

	var gen int64
	require.NoError(t, db.QueryRow(`SELECT model_input_generation FROM sessions WHERE id = ?`,
		session.ID).Scan(&gen))
	require.Equal(t, int64(1), gen)

	// A duplicate promotion replays the accepted row without advancing.
	_, err = store.PromoteInput(ctx, input.ID, "hello")
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT model_input_generation FROM sessions WHERE id = ?`,
		session.ID).Scan(&gen))
	assert.Equal(t, int64(1), gen)
}

func TestPendingInputDoesNotAdvanceGeneration(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)

	_, err = store.EnqueueModelInput(ctx, session.ID, "queued")
	require.NoError(t, err)

	var gen int64
	require.NoError(t, db.QueryRow(`SELECT model_input_generation FROM sessions WHERE id = ?`,
		session.ID).Scan(&gen))
	assert.Equal(t, int64(0), gen)
}

func TestScheduledDeliveryAdvancesGenerationOnce(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	assistant := &transcript.Message{Role: "assistant", Content: ""}
	toolResult := &transcript.Message{Role: "tool", Content: "scheduled work", ToolCallID: "call1"}

	asstID, resultID, inserted, err := store.InsertToolNotificationPairOnce(
		ctx, session.ID, "delivery-1", "fp-1", assistant, toolResult)
	require.NoError(t, err)
	require.True(t, inserted)

	var gen int64
	var boundary sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT model_input_generation, model_input_boundary
		FROM sessions WHERE id = ?`, session.ID).Scan(&gen, &boundary))
	assert.Equal(t, int64(1), gen)
	require.NotNil(t, boundary)
	assert.Equal(t, resultID, boundary.Int64)

	// Duplicate delivery: no advancement.
	_, _, inserted, err = store.InsertToolNotificationPairOnce(
		ctx, session.ID, "delivery-1", "fp-1", assistant, toolResult)
	require.NoError(t, err)
	require.False(t, inserted)
	require.NoError(t, db.QueryRow(`SELECT model_input_generation FROM sessions WHERE id = ?`,
		session.ID).Scan(&gen))
	assert.Equal(t, int64(1), gen)

	// A second distinct delivery advances once more.
	var resultID2 int64
	_, resultID2, inserted, err = store.InsertToolNotificationPairOnce(
		ctx, session.ID, "delivery-2", "fp-2", assistant, toolResult)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, db.QueryRow(`SELECT model_input_generation, model_input_boundary
		FROM sessions WHERE id = ?`, session.ID).Scan(&gen, &boundary))
	assert.Equal(t, int64(2), gen)
	assert.Equal(t, resultID2, boundary.Int64)
	assert.Greater(t, resultID2, asstID)
}

func TestContextResetAdvancesGenerationOnce(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	opening := []*transcript.Message{
		{Role: "user", Content: "scheduled turn"},
	}
	_, inserted, err := store.ResetSessionContextOnce(ctx, session.ID, "reset-1", "fp-r1", opening)
	require.NoError(t, err)
	require.True(t, inserted)

	var gen int64
	var boundary sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT model_input_generation, model_input_boundary
		FROM sessions WHERE id = ?`, session.ID).Scan(&gen, &boundary))
	assert.Equal(t, int64(1), gen)
	require.NotNil(t, boundary)

	var maxID int64
	require.NoError(t, db.QueryRow(`SELECT MAX(id) FROM messages WHERE session_id = ?`,
		session.ID).Scan(&maxID))
	assert.Equal(t, maxID, boundary.Int64)

	_, inserted, err = store.ResetSessionContextOnce(ctx, session.ID, "reset-1", "fp-r1", opening)
	require.NoError(t, err)
	require.False(t, inserted)
	require.NoError(t, db.QueryRow(`SELECT model_input_generation FROM sessions WHERE id = ?`,
		session.ID).Scan(&gen))
	assert.Equal(t, int64(1), gen)
}

func TestCompactionDoesNotAdvanceGeneration(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "hello")
	require.NoError(t, err)
	_, err = store.PromoteInput(ctx, input.ID, "hello")
	require.NoError(t, err)

	msgID, err := store.InsertMessage(ctx, session.ID,
		&transcript.Message{Role: "assistant", Content: "work"})
	require.NoError(t, err)

	_, err = store.ReplaceCompactedMessages(ctx, session.ID, []int64{msgID}, []CompactionEntry{})
	require.NoError(t, err)

	var gen int64
	require.NoError(t, db.QueryRow(`SELECT model_input_generation FROM sessions WHERE id = ?`,
		session.ID).Scan(&gen))
	assert.Equal(t, int64(1), gen)
}

func TestSessionRecordScansGeneration(t *testing.T) {
	store, _, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "hello")
	require.NoError(t, err)
	msg, err := store.PromoteInput(ctx, input.ID, "hello")
	require.NoError(t, err)

	rec, err := store.GetSession(ctx, session.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rec.ModelInputGeneration)
	require.NotNil(t, rec.ModelInputBoundary)
	assert.Equal(t, msg.ID, *rec.ModelInputBoundary)
}
