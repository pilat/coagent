package migrate

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type chatUpgradeOutput struct {
	id, session int64
	owner, key  string
	delivered   bool
}

func TestMigrate_51_SeedsChatSeenAndRemovesProgress(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "chat-focus.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 50)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects(id, work_dir, name) VALUES (1, '/project', 'project');
		INSERT INTO sessions(id, project_id, agent_type, attributes) VALUES
		(1, 1, 'build', '{"manager_id":"one"}'), (2, 1, 'build', '{"manager_id":"one"}'),
		(3, 1, 'build', '{"manager_id":"two"}'), (4, 1, 'build', '{"manager_id":"one"}');
		INSERT INTO manager_bindings(manager_id, driver, attributes, created_at, updated_at) VALUES
		('one', 'telegram', '{"bot_user_id":1,"chat_id":-10042,"topology":"group"}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP),
		('two', 'telegram', '{"bot_user_id":2,"chat_id":99,"topology":"bot"}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
	require.NoError(t, err)

	for _, output := range []chatUpgradeOutput{
		{id: 10, session: 1, owner: "one", key: "first", delivered: true},
		{id: 11, session: 1, owner: "one", key: "progress:delivered", delivered: true},
		{id: 12, session: 1, owner: "one", key: "pending-first"},
		{id: 20, session: 2, owner: "one", key: "second", delivered: true},
		{id: 21, session: 2, owner: "one", key: "progress:pending"},
		{id: 22, session: 2, owner: "one", key: "pending-second"},
		{id: 30, session: 3, owner: "two", key: "other-manager", delivered: true},
		{id: 32, session: 3, owner: "two", key: "pending-other-manager"},
		{id: 40, session: 4, owner: "one", key: "pending-only"},
	} {
		insertChatUpgradeOutput(t, db, output)
	}

	_, err = provider.UpTo(ctx, 51)
	require.NoError(t, err)
	assert.Equal(t, []string{"one -10042 1 11 text", "one -10042 2 20 text", "two 99 3 30 text"},
		stringColumn(t, db, `SELECT manager_id || ' ' || chat_id || ' ' || session_id || ' ' ||
		last_outbox_id || ' ' || typeof(chat_id) FROM chat_seen ORDER BY manager_id, session_id`))
	assert.Zero(t, countRows(t, db, "chat_focus", "1 = 1"))
	assert.Zero(t, countRows(t, db, "session_outbox", "source_key LIKE 'progress:%'"))
	assert.Equal(t, []string{"12", "22", "32", "40"}, stringColumn(t, db, `SELECT CAST(o.id AS TEXT)
		FROM session_outbox o JOIN manager_bindings b ON b.manager_id = json_extract(o.attributes, '$.manager_id')
		LEFT JOIN chat_seen seen ON seen.manager_id = b.manager_id
			AND seen.chat_id = CAST(json_extract(b.attributes, '$.chat_id') AS TEXT) AND seen.session_id = o.session_id
		WHERE o.id > COALESCE(seen.last_outbox_id, 0) ORDER BY o.id`))
	assert.Equal(t, []string{"session_id", "id"},
		stringColumn(t, db, `SELECT name FROM pragma_index_info('idx_session_outbox_session_id') ORDER BY seqno`))
	assert.Equal(t, []string{"other-manager", "pending-other-manager"},
		stringColumn(t, db, `SELECT content FROM session_outbox WHERE session_id = 3 ORDER BY id`))
	assert.Equal(t, []string{"delivered", "pending"},
		stringColumn(t, db, `SELECT state FROM session_outbox WHERE session_id = 3 ORDER BY id`))
}

func insertChatUpgradeOutput(t *testing.T, db *sql.DB, output chatUpgradeOutput) {
	t.Helper()

	kind := "message_persistent"
	if strings.HasPrefix(output.key, "progress:") {
		kind = "message_replaceable"
	}

	state, attempt := "pending", 0
	var lastAttempt, delivered any
	if output.delivered {
		state, attempt = "delivered", 1
		lastAttempt, delivered = "2026-10-01 00:00:00", "2026-10-01 00:00:00"
	}

	_, err := db.ExecContext(t.Context(), `INSERT INTO session_outbox
		(id, session_id, type, content, attributes, source_key, fingerprint, created_at,
		state, attempt_seq, last_attempt_at, delivered_at)
		VALUES (?, ?, ?, ?, ?, ?, 'legacy', CURRENT_TIMESTAMP, ?, ?, ?, ?)`,
		output.id, output.session, kind, output.key, fmt.Sprintf(`{"manager_id":%q}`, output.owner),
		output.key, state, attempt, lastAttempt, delivered)
	require.NoError(t, err)
}
