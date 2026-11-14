package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
)

const verdictConfig = `providers:
    work:
        driver: anthropic
        api_key: sk-ant-verdict-0000
models:
    - id: claude-sonnet-5
      provider: work
    - id: claude-opus-5
      provider: work
`

const verdictCandidate = verdictConfig + "    - id: claude-haiku-4-5\n      provider: work\n"

func TestNoticeConfigRollback(t *testing.T) {
	tests := []struct {
		name       string
		sessionID  int64
		rolledBack bool
		repeat     bool
		oldMarker  bool
		wantNotice bool
	}{
		{name: "rollback notifies the session", sessionID: 7, rolledBack: true, wantNotice: true},
		{name: "repeated outcome posts once", sessionID: 7, rolledBack: true, repeat: true, wantNotice: true},
		{name: "applied posts nothing", sessionID: 7},
		{name: "missing session clears marker without posting", sessionID: 99, rolledBack: true},
		{name: "unattended rollback has nobody to notify", rolledBack: true},
		{name: "old marker clears by decoded identity", sessionID: 7, oldMarker: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops, marker := commitMarker(t, tt.sessionID)
			before, err := os.ReadFile(ops.ConfigPath())
			require.NoError(t, err)
			assert.Equal(t, verdictCandidate, string(before))
			store, db := verdictStore(t)
			pending, err := ops.LoadPending()
			require.NoError(t, err)
			require.NotNil(t, pending)
			if tt.oldMarker {
				data, readErr := os.ReadFile(marker)
				require.NoError(t, readErr)
				var fields map[string]any
				require.NoError(t, json.Unmarshal(data, &fields))
				fields["tool_call_id"] = "old-call"
				fields["tool_name"] = "config_edit"
				fields["summary"] = "replace configuration"
				data, err = json.Marshal(fields)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(marker, data, 0o600))
			}
			var bootErr error
			if tt.rolledBack {
				bootErr = errors.New("model catalog: unknown model")
			}
			outcome, err := ops.ResolvePending(*pending, bootErr)
			require.NoError(t, err)
			require.Equal(t, tt.rolledBack, outcome.RolledBack)
			noticeConfigRollback(t.Context(), store, ops, &outcome)
			if tt.repeat {
				noticeConfigRollback(t.Context(), store, ops, &outcome)
			}
			assert.NoFileExists(t, marker)
			outputs, err := store.ChatOutputs(t.Context(), 7, 0, 10)
			require.NoError(t, err)
			if tt.wantNotice {
				require.Len(t, outputs, 1)
				assert.Equal(t, int64(7), outputs[0].SessionID)
				assert.Equal(t, sessionstore.OutputMessage, outputs[0].Type)
				assert.Contains(t, outputs[0].Content, "model catalog: unknown model")
				assert.Regexp(t, "^⚠️ Config change not applied — ", outputs[0].Content)
			} else {
				assert.Empty(t, outputs)
			}
			var inputs int
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM session_inbox").Scan(&inputs))
			assert.Zero(t, inputs)
			body, err := os.ReadFile(ops.ConfigPath())
			require.NoError(t, err)
			want := verdictCandidate
			if tt.rolledBack {
				want = verdictConfig
			}
			assert.Equal(t, want, string(body))
		})
	}
}

func commitMarker(t *testing.T, sessionID int64) (configops.Service, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(verdictConfig), 0o600))
	ops := configops.New(configPath, filepath.Join(dir, "secrets"))
	staged, v := ops.StageDocument([]byte(verdictCandidate))
	require.True(t, v.Applied, v.Reason())
	v = ops.Commit(staged, configops.Pending{SessionID: sessionID})
	require.True(t, v.Applied, v.Reason())
	return ops, filepath.Join(dir, coagenthome.PendingApplyFileName)
}

func verdictStore(t *testing.T) (*sessionstore.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "verdict.db")
	db, err := migrate.OpenDB(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	provider, err := migrate.NewProvider(db)
	require.NoError(t, err)
	_, err = provider.Up(t.Context())
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "INSERT INTO projects (id, work_dir, name) VALUES (1, ?, 'p')", t.TempDir())
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO sessions (id, project_id, model, agent_type, attributes)
		VALUES (7, 1, 'model', 'build', '{"manager_id":"telegram:main"}')`)
	require.NoError(t, err)
	return sessionstore.NewStore(db), db
}
