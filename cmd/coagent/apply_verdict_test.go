package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// verdictConfig is a config valid enough for a real Stage/Commit round trip.
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

// commitMarker performs a real apply, leaving the marker a boot would find.
func commitMarker(t *testing.T, sessionID int64) (configops.Service, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(verdictConfig), 0o600))

	ops := configops.New(configPath, filepath.Join(dir, "secrets"))

	staged, v := ops.StageDocument([]byte(verdictConfig))
	require.True(t, v.Applied, v.Reason())

	v = ops.Commit(staged, configops.Pending{
		SessionID:  sessionID,
		ToolCallID: "c1",
		ToolName:   tool.IDConfigEdit,
	})
	require.True(t, v.Applied, v.Reason())

	return ops, filepath.Join(dir, coagenthome.PendingApplyFileName)
}

func verdictStore(t *testing.T) *sessionstore.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "verdict.db")
	db, err := migrate.OpenDB(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(t.Context(), db, path))
	_, err = db.ExecContext(t.Context(), "INSERT INTO projects (id, work_dir, name) VALUES (1, ?, 'p')", t.TempDir())
	require.NoError(t, err)
	_, err = db.ExecContext(
		t.Context(),
		"INSERT INTO sessions (id, project_id, model, agent_type) VALUES (7, 1, 'model', 'build')",
	)
	require.NoError(t, err)
	return sessionstore.NewStore(db)
}

func TestDeliverApplyVerdict_MarkerClearedOnlyAfterDelivery(t *testing.T) {
	tests := []struct {
		name string
		id   int64
		fail bool
	}{
		{"sessionless marker has nobody to tell", 0, false},
		{"delivered verdict acknowledges the marker", 7, false},
		{"failed delivery keeps the verdict replayable", 7, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops, marker := commitMarker(t, tt.id)
			store := verdictStore(t)
			pending, err := ops.LoadPending()
			require.NoError(t, err)
			outcome, err := ops.ResolvePending(*pending, nil)
			require.NoError(t, err)
			require.FileExists(t, marker)
			if tt.fail {
				require.NoError(t, store.UpdateSessionStatus(t.Context(), 7, sessionstore.SessionStatusTerminating))
			}
			deliverApplyVerdict(t.Context(), store, configapply.New(ops, store), ops, &outcome)
			if tt.fail {
				require.FileExists(t, marker)
			} else {
				assert.NoFileExists(t, marker)
				if tt.id != 0 {
					rows, err := store.ListPending(t.Context(), tt.id)
					require.NoError(t, err)
					require.Len(t, rows, 1)
					assert.Equal(t, sessionstore.InputSourceCallResult, rows[0].Source)
					assert.Equal(t, "c1", rows[0].Attributes["call_id"])
					assert.Equal(t, tool.IDConfigEdit, rows[0].Attributes["tool_id"])
					assert.Contains(t, rows[0].RawContent, "Config applied:")
				}
			}
		})
	}
}

func TestDeliverApplyVerdict_UndeliverableVerdictConsumesTheMarker(t *testing.T) {
	for _, status := range []sessionstore.SessionStatus{sessionstore.SessionStatusKilled, sessionstore.SessionStatusStopped, sessionstore.SessionStatusStopping} {
		t.Run(string(status), func(t *testing.T) {
			ops, marker := commitMarker(t, 7)
			store := verdictStore(t)
			require.NoError(t, store.UpdateSessionStatus(t.Context(), 7, status))
			pending, err := ops.LoadPending()
			require.NoError(t, err)
			outcome, err := ops.ResolvePending(*pending, nil)
			require.NoError(t, err)
			before, err := os.ReadFile(ops.ConfigPath())
			require.NoError(t, err)
			deliverApplyVerdict(t.Context(), store, configapply.New(ops, store), ops, &outcome)
			assert.NoFileExists(t, marker)
			replay, err := ops.LoadPending()
			require.NoError(t, err)
			assert.Nil(t, replay)
			after, err := os.ReadFile(ops.ConfigPath())
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
	t.Run("gone from the store", func(t *testing.T) {
		ops, marker := commitMarker(t, 99)
		store := verdictStore(t)
		pending, err := ops.LoadPending()
		require.NoError(t, err)
		outcome, err := ops.ResolvePending(*pending, nil)
		require.NoError(t, err)
		deliverApplyVerdict(t.Context(), store, configapply.New(ops, store), ops, &outcome)
		assert.NoFileExists(t, marker)
	})
}

func TestDeliverApplyVerdict_RolledBackUnattendedApplyHasNobodyToTell(t *testing.T) {
	ops, marker := commitMarker(t, 0)
	store := verdictStore(t)
	pending, err := ops.LoadPending()
	require.NoError(t, err)
	outcome, err := ops.ResolvePending(*pending, errors.New("model catalog: unknown model"))
	require.NoError(t, err)
	require.True(t, outcome.RolledBack)
	deliverApplyVerdict(t.Context(), store, configapply.New(ops, store), ops, &outcome)
	assert.NoFileExists(t, marker)
	body, err := os.ReadFile(ops.ConfigPath())
	require.NoError(t, err)
	assert.Equal(t, verdictConfig, string(body))
}

func TestDeliverApplyVerdict_RetryAfterFailureClearsTheMarker(t *testing.T) {
	ops, marker := commitMarker(t, 7)
	store := verdictStore(t)
	applier := configapply.New(ops, store)
	pending, err := ops.LoadPending()
	require.NoError(t, err)
	outcome, err := ops.ResolvePending(*pending, nil)
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(t.Context(), 7, sessionstore.SessionStatusTerminating))
	deliverApplyVerdict(t.Context(), store, applier, ops, &outcome)
	require.FileExists(t, marker)
	require.NoError(t, store.UpdateSessionStatus(t.Context(), 7, sessionstore.SessionStatusActive))
	deliverApplyVerdict(t.Context(), store, applier, ops, &outcome)
	require.NoFileExists(t, marker)
	deliverApplyVerdict(t.Context(), store, applier, ops, &outcome)
	rows, err := store.ListPending(t.Context(), 7)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}
