package migrate

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_55_SessionTitles(t *testing.T) {
	t.Parallel()
	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "titles.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	_, err = provider.UpTo(t.Context(), 53)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO projects(id, work_dir, name) VALUES (1, '/work', 'work')`)
	require.NoError(t, err)
	cases := []struct {
		name, about, title, want, state     string
		inputs                              []string
		ownerless, child, assistant, killed bool
	}{
		{name: "short about", about: "purpose", want: "purpose"},
		{name: "long about", about: strings.Repeat("界", 200), want: strings.Repeat("界", 79) + "…"},
		{name: "multiline about", about: " first\r\nsecond ", want: "first  second"},
		{name: "empty about", about: " \t\r\n ", inputs: []string{"/model x"}},
		{name: "existing title", about: "old purpose", title: "kept", inputs: []string{"new input"}, want: "kept"},
		{name: "skip command", inputs: []string{"/model x", "fix login 500\nmore"}, want: "fix login 500"},
		{name: "about wins", about: "purpose", inputs: []string{"inbox request"}, want: "purpose"},
		{name: "only commands", inputs: []string{"/skill foo", "/model x"}},
		{name: "skip blanks", inputs: []string{"   ", "\n", "deploy staging"}, want: "deploy staging"},
		{name: "ownerless", ownerless: true, inputs: []string{"request"}},
		{name: "child", child: true, inputs: []string{"request"}},
		{name: "assistant", assistant: true, inputs: []string{"request"}},
		{name: "long input", inputs: []string{strings.Repeat("界", 81)}, want: strings.Repeat("界", 79) + "…"},
		{name: "exact limit", inputs: []string{strings.Repeat("界", 80)}, want: strings.Repeat("界", 80)},
		{name: "CRLF", inputs: []string{"first\r\nsecond"}, want: "first"},
		{name: "CR", inputs: []string{"first\rsecond"}, want: "first"},
		{name: "leading blanks", inputs: []string{" \n\t first\nsecond"}, want: "first"},
		{name: "killed", killed: true, inputs: []string{"historical request"}, want: "historical request"},
		{name: "no inputs"},
		{name: "cancelled input", inputs: []string{"original request"}, state: "cancelled", want: "original request"},
		{name: "handled input", inputs: []string{"original request"}, state: "handled", want: "original request"},
	}
	for index, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			id := index + 1
			attrs := map[string]any{"keep": "value"}
			if !tt.ownerless {
				attrs["manager_id"] = "owner"
			}
			if tt.title != "" {
				attrs["title"] = tt.title
			}
			raw, err := json.Marshal(attrs)
			require.NoError(t, err)
			agentType, parent, status := "build", 0, "active"
			if tt.assistant {
				agentType = "assistant"
			}
			if tt.child {
				parent = 1
			}
			if tt.killed {
				status = "killed"
			}
			_, err = db.ExecContext(
				t.Context(),
				`INSERT INTO sessions(id, project_id, attributes, agent_type, parent_id, status)
				VALUES (?, 1, ?, ?, ?, ?)`,
				id,
				string(raw),
				agentType,
				parent,
				status,
			)
			require.NoError(t, err)
			if tt.about != "" {
				_, err = db.ExecContext(t.Context(), `INSERT INTO assistant_work(session_id, about, created_at)
					VALUES (?, ?, CURRENT_TIMESTAMP)`, id, tt.about)
				require.NoError(t, err)
			}
			_, err = db.ExecContext(
				t.Context(),
				`INSERT INTO session_inbox(session_id, source, raw_content, attributes, received_at)
				VALUES (?, 'schedule', 'scheduled first', '{}', CURRENT_TIMESTAMP)`,
				id,
			)
			require.NoError(t, err)
			for _, content := range tt.inputs {
				_, err = db.ExecContext(
					t.Context(),
					`INSERT INTO session_inbox(session_id, source, raw_content, attributes, received_at)
					VALUES (?, 'user', ?, '{}', CURRENT_TIMESTAMP)`,
					id,
					content,
				)
				require.NoError(t, err)
			}
			if tt.state != "" {
				_, err = db.ExecContext(t.Context(), `UPDATE session_inbox SET state = ?,
					resolved_at = CURRENT_TIMESTAMP, resolution_reason = 'historical'
					WHERE session_id = ? AND source = 'user'`, tt.state, id)
				require.NoError(t, err)
			}
		})
	}
	_, err = provider.Up(t.Context())
	require.NoError(t, err)
	assert.False(t, tableExists(t, db, "assistant_work"))
	rows, err := db.QueryContext(
		t.Context(),
		`SELECT json_extract(attributes, '$.title'), json_extract(attributes, '$.keep') FROM sessions ORDER BY id`,
	)
	require.NoError(t, err)
	defer rows.Close()
	var index int
	for rows.Next() {
		var title sql.NullString
		var kept string
		require.NoError(t, rows.Scan(&title, &kept))
		require.Less(t, index, len(cases))
		assert.Equal(t, cases[index].want, title.String, cases[index].name)
		assert.Equal(t, cases[index].want != "", title.Valid, cases[index].name)
		assert.Equal(t, "value", kept)
		index++
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, len(cases), index)
}
