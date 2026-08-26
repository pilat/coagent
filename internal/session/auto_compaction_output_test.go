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
)

func newAutoCompactionStore(t *testing.T) (*contextAutoStore, int64) {
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
	record, err := store.CreateSession(
		ctx, projectID, "model", "", map[string]any{"manager_id": "telegram"},
	)
	require.NoError(t, err)

	return &contextAutoStore{db: db, store: store}, record.ID
}

// Automatic compaction has no input row, so its success outcome must still be
// keyed to its summary message (`compaction:<id>:succeeded`): a crash between
// the summary commit and the outcome enqueue replays as an idempotent no-op
// instead of a duplicate standalone notice.
func TestAutoCompaction_SuccessIsKeyedToItsSummaryMessage(t *testing.T) {
	ctx := context.Background()
	h, sessionID := newAutoCompactionStore(t)

	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}, contextWindow: 32000}
	s := newCompactionTestSvc(llm)
	s.store = h.store
	s.id = sessionID
	s.outputEnabled = true
	s.ms = newMessageStore(h.store, sessionID)
	s.ms.setMessages(oversizedTranscript(32000))

	var notes []string
	contextEventRunner(s, &notes).applyContextEvents(ctx)

	assert.Equal(t, 1, llm.callCount, "auto compaction ran")

	var summaryID int64
	require.NoError(t, h.db.QueryRowContext(ctx, `
		SELECT id FROM messages
		WHERE session_id = ? AND content LIKE '[CONTEXT SUMMARY%'
		ORDER BY id DESC LIMIT 1`, sessionID).Scan(&summaryID))

	var key string
	require.NoError(t, h.db.QueryRowContext(ctx, `
		SELECT COALESCE(source_key, '') FROM session_outbox
		WHERE type = 'message_persistent' AND content = '✅ Context compacted'`).Scan(&key))
	assert.Equal(t,
		"compaction:"+strconv.FormatInt(summaryID, 10)+":succeeded", key)
}

type contextAutoStore struct {
	db    *sql.DB
	store sessionstore.Store
}
