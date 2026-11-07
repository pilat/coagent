package schedule

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

type staleCronStore struct {
	Store
	due *Schedule
}

func (s staleCronStore) ListDueCronSchedules(context.Context, time.Time) ([]*Schedule, error) {
	return []*Schedule{s.due}, nil
}

type cronCommitSender struct {
	t             *testing.T
	store         Store
	sessionID     int64
	notifications int
}

func (s *cronCommitSender) NotifySession(id int64, n sessionevent.Notification) {
	s.t.Helper()
	rows, err := s.store.ListSchedules(s.t.Context(), id)
	require.NoError(s.t, err)
	require.Len(s.t, rows, 1)
	require.NotNil(s.t, rows[0].lastFiredAt, "notification must follow the acknowledgement commit")
	assert.Equal(s.t, s.sessionID, id)
	assert.Equal(s.t, sessionevent.NotifyInputReceived, n.Type)
	s.notifications++
}

func TestExecutor_CronAckRetryKeepsCanonicalIdentityAndPayload(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "cron.db")
	db, err := migrate.OpenDB(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	provider, err := migrate.NewProvider(db)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	sessions := sessionstore.NewStore(db)
	store := NewStore(db, sessions)
	root := seedOneShotSession(ctx, t, db)
	due, err := store.AddSchedule(ctx, root, "* * * * *", nil, "canonical retry", false, ScheduleMetadata{})
	require.NoError(t, err)
	sender := &cronCommitSender{t: t, store: store, sessionID: root}
	executor := NewExecutor(staleCronStore{Store: store, due: due}, sender)
	firstTick := time.Date(2026, time.August, 14, 12, 0, 5, 0, time.UTC)
	require.NoError(t, sessions.UpdateSessionStatus(ctx, root, sessionstore.SessionStatusKilled))
	require.NoError(t, executor.fireCronSchedules(ctx, firstTick, zap.NewNop()))
	assert.Zero(t, sender.notifications)
	rows, err := store.ListSchedules(ctx, root)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].lastFiredAt)
	assert.Zero(t, rows[0].fireCount)
	require.NoError(t, sessions.UpdateSessionStatus(ctx, root, sessionstore.SessionStatusActive))
	require.NoError(t, executor.fireCronSchedules(ctx, firstTick.Add(20*time.Second), zap.NewNop()))
	require.NoError(t, executor.fireCronSchedules(ctx, firstTick.Add(40*time.Second), zap.NewNop()))
	inputs, err := sessions.ListPending(ctx, root)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	assert.Equal(
		t,
		"Schedule tick #1 (cron: * * * * *). Current time: 2026-08-14T12:00:00Z\n\nTask: canonical retry",
		inputs[0].RawContent,
	)
	assert.Equal(t, 1, sender.notifications)
}
