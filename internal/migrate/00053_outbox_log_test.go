package migrate

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

func TestMigrate_53_PreservesOutputIDsSequenceAndWatermarks(t *testing.T) {
	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "outbox-log.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	_, err = provider.UpTo(t.Context(), 52)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO projects(id, work_dir, name) VALUES (1, '/work', 'work');
		INSERT INTO sessions(id, project_id, agent_type, attributes) VALUES (1, 1, 'build', '{"manager_id":"owner"}');
		INSERT INTO session_outbox(id, session_id, type, content, attributes, created_at) VALUES
		(10, 1, 'message_persistent', 'first', '{"manager_id":"owner"}', CURRENT_TIMESTAMP),
		(20, 1, 'message_replaceable', 'second', '{"manager_id":"owner"}', CURRENT_TIMESTAMP),
		(90, 1, 'message_replaceable', 'removed progress', '{"manager_id":"owner"}', CURRENT_TIMESTAMP);
		DELETE FROM session_outbox WHERE id = 90;
		INSERT INTO chat_seen VALUES ('owner', 'chat', 1, 90)`)
	require.NoError(t, err)
	_, err = provider.UpTo(t.Context(), 53)
	require.NoError(t, err)
	assert.Equal(t, []string{"10:message:first", "20:message:second"}, stringColumn(t, db,
		`SELECT id || ':' || type || ':' || content FROM session_outbox ORDER BY id`))
	assert.Equal(t, []string{"90"}, stringColumn(t, db, `SELECT CAST(last_outbox_id AS TEXT) FROM chat_seen`))
	assert.False(t, tableExists(t, db, "manager_bindings"))
	assert.False(t, columnExists(t, db, "sessions", "manager_reply_pending"))
	assert.Equal(
		t,
		[]string{
			"id",
			"session_id",
			"type",
			"content",
			"attributes",
			"source_key",
			"fingerprint",
			"releases_input",
			"created_at",
		},
		stringColumn(t, db, `SELECT name FROM pragma_table_info('session_outbox') ORDER BY cid`),
	)
	assert.Equal(
		t,
		[]string{"idx_session_outbox_session_id", "idx_session_outbox_source_key"},
		stringColumn(
			t,
			db,
			`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'session_outbox' ORDER BY name`,
		),
	)
	result, err := db.ExecContext(
		t.Context(),
		`INSERT INTO session_outbox(session_id, type, content, attributes, created_at)
		VALUES (1, 'message', 'new', '{}', CURRENT_TIMESTAMP)`,
	)
	require.NoError(t, err)
	id, err := result.LastInsertId()
	require.NoError(t, err)
	assert.Greater(t, id, int64(90))
	assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
}

func TestMigrate_54_ReinsertsLegacySourceKeysThroughRealStore(t *testing.T) {
	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "fingerprints.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	_, err = provider.UpTo(t.Context(), 52)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO projects(id, work_dir, name) VALUES (1, '/work', 'work');
		INSERT INTO sessions(id, project_id, agent_type, attributes) VALUES
		(1, 1, 'build', '{"manager_id":"owner"}'), (9007199254740993, 1, 'build', '{"manager_id":"owner"}')`)
	require.NoError(t, err)
	fixtures := []struct {
		legacyType string
		draft      sessionstore.OutputDraft
	}{
		{
			"message_persistent",
			sessionstore.OutputDraft{
				Type:          sessionstore.OutputMessage,
				Content:       "final",
				ReleasesInput: true,
				Attributes:    map[string]any{"source": "agent"},
			},
		},
		{"message_replaceable", sessionstore.OutputDraft{Type: sessionstore.OutputMessage, Content: "narration"}},
		{
			"message_persistent",
			sessionstore.OutputDraft{
				Type:       sessionstore.OutputMessage,
				Content:    "scheduled",
				Attributes: map[string]any{"source": "scheduler"},
			},
		},
		{
			"session_opened",
			sessionstore.OutputDraft{
				Type:       sessionstore.OutputSessionOpened,
				Attributes: map[string]any{"name": "work", "work_dir": "/work"},
			},
		},
		{
			"session_replaced",
			sessionstore.OutputDraft{
				Type:      sessionstore.OutputSessionReplaced,
				SessionID: 9007199254740993,
				Attributes: map[string]any{
					"old_session_id": int64(1),
					"new_session_id": int64(9007199254740993),
					"name":           "work",
					"work_dir":       "/work",
				},
			},
		},
		{
			"session_closed",
			sessionstore.OutputDraft{
				Type:       sessionstore.OutputSessionClosed,
				Attributes: map[string]any{"reason": "killed", "cancelled_processes": float64(2)},
			},
		},
	}
	for i := range fixtures {
		draft := &fixtures[i].draft
		if draft.SessionID == 0 {
			draft.SessionID = 1
		}
		draft.SourceKey = fixtures[i].legacyType + ":" + draft.Content
		attributes := map[string]any{
			"manager_id":             "owner",
			"message_ids":            []string{"old-receipt"},
			"waiting":                []any{},
			"waiting_identity":       []any{},
			"progress_revision":      "old",
			"model_input_generation": 7,
		}
		maps.Copy(attributes, draft.Attributes)
		encoded, err := json.Marshal(attributes)
		require.NoError(t, err)
		_, err = db.ExecContext(
			t.Context(),
			`INSERT INTO session_outbox
			(id, session_id, type, content, attributes, source_key, fingerprint, releases_input, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 'old-fingerprint', ?, CURRENT_TIMESTAMP)`,
			i+10,
			draft.SessionID,
			fixtures[i].legacyType,
			draft.Content,
			string(encoded),
			draft.SourceKey,
			draft.ReleasesInput,
		)
		require.NoError(t, err)
	}
	_, err = provider.Up(t.Context())
	require.NoError(t, err)
	store := sessionstore.NewStore(db)
	for i, fixture := range fixtures {
		draft := fixture.draft
		require.NoError(
			t,
			db.QueryRowContext(t.Context(), `SELECT fingerprint FROM session_outbox WHERE id = ?`, i+10).
				Scan(&draft.Fingerprint),
		)
		assert.NotEqual(t, "old-fingerprint", draft.Fingerprint)
		if !draft.ReleasesInput {
			assert.Equal(
				t,
				sessionstore.OutputFingerprint(draft.Type, draft.Content, draft.SessionID, draft.Attributes),
				draft.Fingerprint,
			)
		}
		commit, err := store.EnqueueOutput(t.Context(), draft)
		require.NoError(t, err)
		require.NotNil(t, commit)
		assert.True(t, commit.Existing)
		assert.Equal(t, int64(i+10), commit.OutputID)
	}
	assert.Equal(t, len(fixtures), countRows(t, db, "session_outbox", "1 = 1"))
}
