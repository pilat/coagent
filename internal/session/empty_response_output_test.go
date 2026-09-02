package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
)

// The plan's output producer matrix requires an empty-response break to emit a
// persistent message output, so a manager-bound root's CLI terminal is told it
// went idle (and sees the pause notice) instead of staying stuck busy.
func TestRunLoopEmptyResponseEmitsPersistentOutput(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/session.db"
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	_, err = db.ExecContext(ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test")
	require.NoError(t, err)

	store := sessionstore.NewStore(db)
	record, err := store.CreateSession(ctx, 1, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)

	llm := &loopScriptLLM{responses: []*llmwire.Response{{}}}
	notifier := &loopNotifier{}

	agent := newTestAgent()
	agent.llmClient = llm
	agent.outputEnabled = true
	agent.store = store
	agent.outputStore = store
	agent.id = record.ID
	agent.ms = newMessageStore(store, record.ID, store)

	result, err := runLoop(ctx, agent, loopOptions{Notify: notifier.fn}, iterationGuard(20))
	require.NoError(t, err)
	assert.Equal(t, 6, result.Iterations)

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM session_outbox
		WHERE type = 'message_persistent'`).Scan(&count))
	assert.Equal(t, 1, count, "the empty-response pause notice must be a durable persistent output")
}
