package migrate

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_00047_AssistantDesk(t *testing.T) {
	for _, previous := range []bool{false, true} {
		name := "fresh"
		if previous {
			name = "from_46"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "assistant.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			provider := newProvider(t, db)

			if previous {
				_, err = provider.UpTo(ctx, 46)
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/p', 'p');
					INSERT INTO sessions (id, project_id, model, agent_type) VALUES (1, 1, 'm', 'build')`)
				require.NoError(t, err)
			}

			_, err = provider.UpTo(ctx, 47)
			require.NoError(t, err)

			for _, table := range []string{"assistant_desk", "assistant_tape", "assistant_sends", "assistant_turns"} {
				assert.True(t, columnExists(t, db, table, "session_id"), table)
			}

			if previous {
				var model string
				require.NoError(t, db.QueryRowContext(ctx, `SELECT model FROM sessions WHERE id = 1`).Scan(&model))
				assert.Equal(t, "m", model)
			}

			_, err = db.ExecContext(
				ctx,
				`INSERT INTO assistant_desk (session_id, updated_at) VALUES (999, CURRENT_TIMESTAMP)`,
			)
			require.ErrorContains(t, err, "FOREIGN KEY")
		})
	}
}
