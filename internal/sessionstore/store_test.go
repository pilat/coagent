package sessionstore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/transcript"
)

// A switch to a model with no effort choice must land an empty level, not a medium
func TestUpdateSessionModelWritesTheLevelVerbatim(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)

	rec, err := store.CreateSession(ctx, projectID, "model-a", "high", nil)
	require.NoError(t, err)

	require.NoError(t, store.UpdateSessionModel(ctx, rec.ID, "model-b", ""))

	var raw string
	require.NoError(t,
		db.QueryRowContext(ctx, `SELECT reasoning_level FROM sessions WHERE id = ?`, rec.ID).Scan(&raw))
	assert.Empty(t, raw)

	reloaded, err := store.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	assert.Empty(t, reloaded.ReasoningLevel, "the read path must not invent a level either")

	require.NoError(t, store.UpdateSessionModel(ctx, rec.ID, "model-c", "low"))
	require.NoError(t,
		db.QueryRowContext(ctx, `SELECT reasoning_level FROM sessions WHERE id = ?`, rec.ID).Scan(&raw))
	assert.Equal(t, "low", raw)
}

func TestUpdateSessionStatusRejectsMissingSession(t *testing.T) {
	store, _, _ := newTestStore(t)

	err := store.UpdateSessionStatus(context.Background(), 999_999, SessionStatusCompleted)
	require.Error(t, err)
	assert.ErrorContains(t, err, "session 999999 not found")
}

func TestMarkSessionKilledMarksTheTerminalStatus(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)

	require.NoError(t, store.MarkSessionKilled(ctx, record.ID))
	killed, err := store.GetSession(ctx, record.ID)
	require.NoError(t, err)
	assert.Equal(t, SessionStatusKilled, killed.Status)
	assert.NotNil(t, killed.KilledAt)
}

func TestSessionStatusRejectsUnknownVocabulary(t *testing.T) {
	store, _, projectID := newTestStore(t)
	ctx := context.Background()

	rec, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	assert.Equal(t, SessionStatusActive, rec.Status, "returned record must match the database default")

	err = store.UpdateSessionStatus(ctx, rec.ID, SessionStatus("running"))
	require.ErrorContains(t, err, "invalid session status")

	reloaded, err := store.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, SessionStatusActive, reloaded.Status)
}

func TestReasoningRawSurvivesReload(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	rec, err := store.CreateSession(ctx, projectID, "claude-opus-5", "high", nil)
	require.NoError(t, err)

	envelope := json.RawMessage(`{"model":"claude-opus-5","payload":[{"type":"thinking","signature":"sig"}]}`)

	_, err = store.InsertMessage(ctx, rec.ID, &transcript.Message{
		Role:         "assistant",
		Content:      "thinking out loud",
		ReasoningRaw: envelope,
	})
	require.NoError(t, err)

	_, err = store.InsertMessage(ctx, rec.ID, &transcript.Message{Role: "user", Content: "next"})
	require.NoError(t, err)

	loaded, err := store.LoadActiveMessages(ctx, rec.ID)
	require.NoError(t, err)
	require.Len(t, loaded, 2)

	assert.JSONEq(t, string(envelope), string(loaded[0].ReasoningRaw))
	assert.Nil(t, loaded[1].ReasoningRaw, "a message without reasoning stores nothing")
}

func TestAttachmentsSurviveReload(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	rec, err := store.CreateSession(ctx, projectID, "claude-opus-5", "high", nil)
	require.NoError(t, err)

	refs := json.RawMessage(`[{"path":"/tmp/coagent-a1b2.png","mime":"image/png","size":1234}]`)

	_, err = store.InsertMessage(ctx, rec.ID, &transcript.Message{
		Role:        llmwire.RoleTool,
		Content:     "[/tmp/coagent-a1b2.png]",
		ToolCallID:  "call-1",
		Attachments: refs,
	})
	require.NoError(t, err)

	_, err = store.InsertMessage(ctx, rec.ID, &transcript.Message{Role: llmwire.RoleUser, Content: "next"})
	require.NoError(t, err)

	loaded, err := store.LoadActiveMessages(ctx, rec.ID)
	require.NoError(t, err)
	require.Len(t, loaded, 2)

	assert.JSONEq(t, string(refs), string(loaded[0].Attachments))
	assert.Nil(t, loaded[1].Attachments, "a message without attachments stores nothing")
}
