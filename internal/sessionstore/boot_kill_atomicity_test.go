package sessionstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The killed transition and its close output must commit in one transaction: a
// crash (or any insert failure) between them would leave a killed root whose
// close obligation is lost forever, because reconciliation never re-selects it.
func TestOutputStore_BootKillCloseIsAtomicWithKilledTransition(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)

	record, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{"manager_id": "telegram"},
		Name: "project", WorkDir: "/work/project",
	})
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(ctx, record.ID, SessionStatusTerminating))

	// Inject the crash between the killed UPDATE and the close-row INSERT.
	_, err = db.ExecContext(ctx, `
		CREATE TRIGGER fail_session_closed BEFORE INSERT ON session_outbox
		WHEN NEW.type = 'session_closed'
		BEGIN SELECT RAISE(ABORT, 'injected close failure'); END`)
	require.NoError(t, err)

	require.Error(t, store.KillTerminatingSessions(ctx))

	var status string
	var killedAt *time.Time
	require.NoError(t, db.QueryRowContext(
		ctx, `SELECT status, killed_at FROM sessions WHERE id = ?`, record.ID,
	).Scan(&status, &killedAt))
	assert.Equal(t, string(SessionStatusTerminating), status,
		"the failed close insert must roll back the killed transition too")
	assert.Nil(t, killedAt)

	_, err = db.ExecContext(ctx, `DROP TRIGGER fail_session_closed`)
	require.NoError(t, err)

	// The retry after repair completes both sides of the transition.
	require.NoError(t, store.KillTerminatingSessions(ctx))

	require.NoError(t, db.QueryRowContext(
		ctx, `SELECT status, killed_at FROM sessions WHERE id = ?`, record.ID,
	).Scan(&status, &killedAt))
	assert.Equal(t, string(SessionStatusKilled), status)
	require.NotNil(t, killedAt)

	var closeRows int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND type = 'session_closed'`, record.ID).Scan(&closeRows))
	assert.Equal(t, 1, closeRows)
}
