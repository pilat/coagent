package progressruntime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestReconcileProgressSafelySurvivesStorePanic(t *testing.T) {
	t.Parallel()
	runtime := New(nil, sessionbus.New())
	ctx := logger.ToContext(context.Background(), zap.NewNop())
	assert.Equal(t, SilenceInterval, runtime.Reconcile(ctx, time.Now().UTC()))
}

func TestReconcileProgressSelectsDeadlineByMainModelActivity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		working bool
		want    time.Duration
	}{
		{"main model working", true, 30 * time.Second},
		{"main model idle", false, 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			path := filepath.Join(t.TempDir(), "progress.db")
			db, err := migrate.OpenDB(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			require.NoError(t, migrate.Run(t.Context(), db, path))
			store := sessionstore.NewStore(db)
			project, err := store.GetOrCreateProject(t.Context(), t.TempDir())
			require.NoError(t, err)
			root, err := store.CreateSession(t.Context(), project, "model", "", map[string]any{"manager_id": "test"})
			require.NoError(t, err)
			_, err = store.Enqueue(
				t.Context(),
				sessionstore.Input{SessionID: root.ID, Source: sessionstore.InputSourceUser, Content: "work"},
			)
			require.NoError(t, err)
			_, err = db.ExecContext(
				t.Context(),
				"UPDATE sessions SET episode_started_at = ? WHERE id = ?",
				now,
				root.ID,
			)
			require.NoError(t, err)
			runtime := New(store, sessionbus.New())
			runtime.SetLive(root.ID, Live{Active: true, Working: tt.working})
			assert.Equal(t, tt.want, runtime.Reconcile(t.Context(), now))
		})
	}
}
