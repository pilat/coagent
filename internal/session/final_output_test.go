package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func newFinalOutputStore(t *testing.T) (*sql.DB, sessionstore.Store, int64) {
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

	store := sessionstore.NewStore(db)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)

	return db, store, record.ID
}

// The max-iteration promotion targets the LAST assistant message: when that
// message was already committed as terminal, its same-key replay must be a
// no-op — never a second persistent row resurrecting an older turn's text.
func TestMessageStore_FinalPromotionTargetsLastAssistantMessage(t *testing.T) {
	ctx := context.Background()
	db, store, sessionID := newFinalOutputStore(t)
	ms := newMessageStore(store, sessionID, store)

	intermediate, err := storedMessage(&llmwire.Message{
		Role: llmwire.RoleAssistant, Content: "reading the file first",
		ToolCalls: []llmwire.ToolCall{{ID: "call-1", Name: "read", Arguments: []byte("{}")}},
	})
	require.NoError(t, err)
	_, err = store.CommitAcceptedResponseDisposition(ctx, sessionstore.AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Kind: sessionstore.ResponseDispositionToolCall,
		Message:    intermediate,
		OutputType: sessionstore.OutputMessageReplaceable, Output: "reading the file first",
	})
	require.NoError(t, err)
	require.NoError(t, ms.reloadMessages(ctx))
	require.NoError(t, ms.addToolResult(ctx, "call-1", "read", "file body"))

	candidate, err := store.CommitAcceptedResponseDisposition(ctx, sessionstore.AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Kind: sessionstore.ResponseDispositionCandidate,
		Message:             &transcript.Message{Role: llmwire.RoleAssistant, Content: "the final answer"},
		Nudge:               &transcript.Message{Role: llmwire.RoleUser, Content: "Check completion"},
		ManagerReplyPending: true,
	})
	require.NoError(t, err)
	_, err = store.CommitAcceptedResponseDisposition(ctx, sessionstore.AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Kind: sessionstore.ResponseDispositionConfirmed,
		Message:             &transcript.Message{Role: llmwire.RoleAssistant, Content: "confirmed"},
		ExpectedCandidateID: candidate.MessageID, ManagerReplyPending: true,
		OutputType: sessionstore.OutputMessagePersistent, Output: "✅ the final answer",
	})
	require.NoError(t, err)
	require.NoError(t, ms.reloadMessages(ctx))

	lastID := ms.rowIDs[len(ms.rowIDs)-1]
	require.NotZero(t, lastID)

	require.NoError(t, ms.enqueueFinalAssistantOutput(ctx, "✅ the final answer"))

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM session_outbox WHERE type = 'message_persistent'`).Scan(&count))
	assert.Equal(t, 1, count,
		"promotion of an already-terminal answer must not enqueue a stale duplicate")

	var sourceKey string
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COALESCE(source_key, '') FROM session_outbox
		WHERE type = 'message_persistent'`).Scan(&sourceKey))
	assert.Equal(t, "message:"+strconv.FormatInt(lastID, 10)+":final", sourceKey)
}
