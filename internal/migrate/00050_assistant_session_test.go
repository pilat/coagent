package migrate

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test00050AssistantSession(t *testing.T) {
	for _, previous := range []bool{false, true} {
		name := "fresh"
		if previous {
			name = "from_49"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "assistant-session.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			provider := newProvider(t, db)
			if previous {
				_, err = provider.UpTo(t.Context(), 49)
				require.NoError(t, err)
				_, err = db.ExecContext(t.Context(), `INSERT INTO projects(id,work_dir,name) VALUES(1,'/p','p');
INSERT INTO sessions(id,project_id,model,agent_type,attributes) VALUES
(1,1,'model','assistant','{"manager_id":"owner"}'),(2,1,'model','build','{"manager_id":"owner"}');
INSERT INTO session_inbox(id,session_id,source,raw_content,attributes,received_at,state)
VALUES(1,2,'user','work','{}',CURRENT_TIMESTAMP,'pending');
INSERT INTO messages(id,session_id,role,content,created_at) VALUES(1,2,'assistant','kept message',CURRENT_TIMESTAMP);
INSERT INTO assistant_tape(id,session_id,at,kind,text) VALUES(1,1,CURRENT_TIMESTAMP,'say','discarded tape');
INSERT INTO assistant_turns(session_id,input_id,call,phase,request,response,outcome,created_at)
VALUES(1,1,1,'main','desk','answer','ok',CURRENT_TIMESTAMP);
INSERT INTO assistant_memory(id,session_id,text,created_at,updated_at) VALUES(1,1,'kept memory',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO assistant_focus(id,session_id,text,sessions,created_at,updated_at) VALUES(1,1,'kept focus','["2"]',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO assistant_work(session_id,about,created_at) VALUES(2,'kept work',CURRENT_TIMESTAMP);
INSERT INTO assistant_sends(input_id,assistant_session_id,session_id,created_at) VALUES(1,1,2,CURRENT_TIMESTAMP);
INSERT INTO assistant_search(rowid,body,scope) VALUES(4,'kept message',2),(5,'discarded tape',1),(6,'kept memory',1),(7,'kept focus',1);
UPDATE assistant_search_state SET message_id = 1 WHERE id = 1;`)
				require.NoError(t, err)
			}
			_, err = provider.Up(t.Context())
			require.NoError(t, err)
			for _, table := range []string{"assistant_tape", "assistant_turns"} {
				var count int
				require.NoError(
					t,
					db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, table).
						Scan(&count),
				)
				assert.Zero(t, count, table)
			}
			for table, column := range map[string]string{
				"assistant_memory": "session_id", "assistant_focus": "session_id", "assistant_work": "session_id",
				"assistant_sends": "input_id", "assistant_search_state": "message_id",
			} {
				assert.True(t, columnExists(t, db, table, column), table)
			}
			assert.Empty(t, stringColumn(t, db, `SELECT body FROM assistant_search WHERE rowid % 4 = 1`))
			if previous {
				assert.Equal(
					t,
					[]string{"kept message", "kept memory", "kept focus"},
					stringColumn(t, db, `SELECT body FROM assistant_search ORDER BY rowid`),
				)
				for _, table := range []string{"assistant_memory", "assistant_focus", "assistant_work", "assistant_sends"} {
					assert.Equal(t, []string{"1"}, stringColumn(t, db, `SELECT COUNT(*) FROM `+table), table)
				}
				assert.Equal(t, []string{"1"}, stringColumn(t, db, `SELECT message_id FROM assistant_search_state`))
				_, err = db.ExecContext(
					t.Context(),
					`INSERT INTO sessions(project_id,model,agent_type,attributes) VALUES(1,'model','assistant','{"manager_id":"owner"}')`,
				)
				require.ErrorContains(t, err, "UNIQUE")
			}
		})
	}
}
