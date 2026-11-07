package migrate

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

type scheduleClaimUpgrade struct {
	db       *sql.DB
	sessions *sessionstore.Store
	store    schedule.Store
	entry    *schedule.Schedule
	input    sessionstore.Input
	minute   time.Time
}

func TestMigrate_00050_PreventsScheduleClaimReplay(t *testing.T) {
	for _, source := range []string{"inbox", "legacy"} {
		for _, kind := range []string{"one-shot", "fresh one-shot", "cron", "sleep"} {
			t.Run(source+"/"+kind, func(t *testing.T) {
				t.Parallel()
				f := newScheduleClaimUpgrade(t, kind)
				f.claim(t, source, f.input.DeliveryKey)
				require.NoError(
					t,
					f.sessions.UpdateSessionStatus(t.Context(), f.entry.SessionID(), sessionstore.SessionStatusStopped),
				)
				f.sessions.TakeWoken()
				queries := []string{
					"SELECT * FROM session_inbox ORDER BY id",
					"SELECT * FROM messages ORDER BY id",
					"SELECT * FROM sessions ORDER BY id",
				}
				before := make([][]map[string]sql.NullString, len(queries))
				for i, query := range queries {
					before[i] = dumpRows(t, f.db, query)
				}

				provider, err := NewProvider(f.db)
				require.NoError(t, err)
				_, err = provider.UpTo(t.Context(), 50)
				require.NoError(t, err)
				rows, err := f.store.ListSchedules(t.Context(), f.entry.SessionID())
				require.NoError(t, err)
				if kind == "cron" {
					require.Len(t, rows, 1)
					require.NotNil(t, rows[0].LastFiredAt())
					assert.True(t, f.minute.Equal(*rows[0].LastFiredAt()))
					f.assertFireCount(t)
				} else {
					assert.Empty(t, rows)
				}
				applied, err := f.redeliver(t)
				require.NoError(t, err)
				assert.False(t, applied, "an upgraded claim must suppress another delivery")
				for i, query := range queries {
					assert.Equal(t, before[i], dumpRows(t, f.db, query), query)
				}
				assert.Empty(t, f.sessions.TakeWoken())
				assert.False(t, tableExists(t, f.db, "session_deliveries"))
			})
		}
	}
}

func TestMigrate_00050_CronKeepsLatestClaimAndPreservesFireCount(t *testing.T) {
	for _, offset := range []time.Duration{-time.Minute, 0, 20 * time.Second, time.Minute} {
		t.Run(offset.String(), func(t *testing.T) {
			t.Parallel()
			f := newScheduleClaimUpgrade(t, "cron")
			latest := f.minute.Add(time.Minute)
			previous := latest.Add(offset)
			_, err := f.db.ExecContext(
				t.Context(),
				`UPDATE schedules SET last_fired_at = ? WHERE id = ?`,
				previous,
				f.entry.ID(),
			)
			require.NoError(t, err)
			f.claim(t, "inbox", f.input.DeliveryKey)
			f.claim(t, "legacy", fmt.Sprintf("schedule:cron:%d:%s", f.entry.ID(), latest.Format("20060102T1504Z")))
			f.claim(
				t,
				"legacy",
				fmt.Sprintf("schedule:cron:%d:%s", f.entry.ID(), f.minute.Add(-time.Minute).Format("20060102T1504Z")),
			)

			provider, err := NewProvider(f.db)
			require.NoError(t, err)
			_, err = provider.UpTo(t.Context(), 50)
			require.NoError(t, err)
			rows, err := f.store.ListSchedules(t.Context(), f.entry.SessionID())
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.NotNil(t, rows[0].LastFiredAt())
			want := latest
			if previous.After(latest) {
				want = previous
			}
			assert.True(t, want.Equal(*rows[0].LastFiredAt()))
			f.assertFireCount(t)
		})
	}
}

func TestMigrate_00050_ClaimsDoNotAcknowledgeAnotherSession(t *testing.T) {
	for _, kind := range []string{"one-shot", "cron", "sleep"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newScheduleClaimUpgrade(t, kind)
			project, err := f.sessions.GetOrCreateProject(t.Context(), t.TempDir())
			require.NoError(t, err)
			other, err := f.sessions.CreateSession(t.Context(), project, "model", "", nil)
			require.NoError(t, err)
			f.input.SessionID = other.ID
			f.claim(t, "inbox", f.input.DeliveryKey)
			f.claim(t, "legacy", f.input.DeliveryKey)
			before := dumpRows(t, f.db, `SELECT * FROM schedules ORDER BY id`)

			provider, err := NewProvider(f.db)
			require.NoError(t, err)
			_, err = provider.UpTo(t.Context(), 50)
			require.NoError(t, err)
			assert.Equal(t, before, dumpRows(t, f.db, `SELECT * FROM schedules ORDER BY id`))
		})
	}
}

func newScheduleClaimUpgrade(t *testing.T, kind string) *scheduleClaimUpgrade {
	t.Helper()
	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "claim-upgrade.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	provider, err := NewProvider(db)
	require.NoError(t, err)
	_, err = provider.UpTo(t.Context(), 49)
	require.NoError(t, err)
	sessions := sessionstore.NewStore(db)
	project, err := sessions.GetOrCreateProject(t.Context(), t.TempDir())
	require.NoError(t, err)
	root, err := sessions.CreateSession(t.Context(), project, "model", "", nil)
	require.NoError(t, err)
	f := &scheduleClaimUpgrade{
		db:       db,
		sessions: sessions,
		store:    schedule.NewStore(db, sessions),
		minute:   time.Now().UTC().Truncate(time.Minute),
	}
	f.addSchedule(t, root.ID, kind)
	return f
}

func (f *scheduleClaimUpgrade) addSchedule(t *testing.T, root int64, kind string) {
	t.Helper()
	cron, at := "", &f.minute
	meta := schedule.ScheduleMetadata{}
	f.input = sessionstore.Input{
		SessionID:  root,
		Source:     sessionstore.InputSourceSchedule,
		Content:    "already delivered",
		Attributes: map[string]any{"fresh": kind == "fresh one-shot"},
	}
	if kind == "cron" {
		cron, at = "* * * * *", nil
	}
	if kind == "sleep" {
		meta.ToolCallID = "sleep-call"
		f.input.Source = sessionstore.InputSourceCallResult
		f.input.Attributes = map[string]any{"call_id": "sleep-call", "tool_id": "sleep"}
		_, err := f.sessions.Commit(t.Context(), sessionstore.Commit{SessionID: root, Messages: []*transcript.Message{
			{Role: "assistant", ToolCalls: []byte(`[{"ID":"sleep-call","Name":"sleep","Arguments":"e30="}]`)},
		}})
		require.NoError(t, err)
	}
	var err error
	f.entry, err = f.store.AddSchedule(t.Context(), root, cron, at, f.input.Content, kind == "fresh one-shot", meta)
	require.NoError(t, err)
	f.input.DeliveryKey = fmt.Sprintf("schedule:one-shot:%d", f.entry.ID())
	if kind == "cron" {
		f.input.DeliveryKey = fmt.Sprintf("schedule:cron:%d:%s", f.entry.ID(), f.minute.Format("20060102T1504Z"))
	}
	if kind == "sleep" {
		f.input.DeliveryKey = fmt.Sprintf("sleep:%d", f.entry.ID())
	}
	_, err = f.db.ExecContext(t.Context(), `UPDATE schedules SET fire_count = 7 WHERE id = ?`, f.entry.ID())
	require.NoError(t, err)
}

func (f *scheduleClaimUpgrade) claim(t *testing.T, source, key string) {
	t.Helper()
	if source == "legacy" {
		_, err := f.db.ExecContext(
			t.Context(),
			`INSERT INTO session_deliveries(session_id, delivery_id, kind, fingerprint, delivered_at)
			VALUES (?, ?, 'tool_notification', 'legacy', ?)`,
			f.input.SessionID,
			key,
			f.minute.Add(2*time.Minute),
		)
		require.NoError(t, err)
		return
	}
	input := f.input
	input.DeliveryKey = key
	result, err := f.sessions.Enqueue(t.Context(), input)
	require.NoError(t, err)
	require.True(t, result.Applied)
}

func (f *scheduleClaimUpgrade) redeliver(t *testing.T) (bool, error) {
	t.Helper()
	if f.entry.Sleep() {
		return f.store.DeliverSleep(t.Context(), f.entry)
	}
	if f.entry.CronExpr() != "" {
		return f.store.DeliverCron(t.Context(), f.entry, f.input.Content, f.input.Attributes, f.minute, f.minute)
	}
	return f.store.DeliverOneShot(t.Context(), f.entry, f.input.Content, f.input.Attributes)
}

func (f *scheduleClaimUpgrade) assertFireCount(t *testing.T) {
	t.Helper()
	var count int
	require.NoError(
		t,
		f.db.QueryRowContext(t.Context(), `SELECT fire_count FROM schedules WHERE id = ?`, f.entry.ID()).Scan(&count),
	)
	assert.Equal(t, 7, count)
}

func TestMigrate_00050_DropsSessionDeliveriesPreservingOtherTables(t *testing.T) {
	t.Parallel()

	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "drop-deliveries.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	_, err = provider.UpTo(t.Context(), 49)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO projects(id, work_dir, name) VALUES (1, '/work', 'work');
		INSERT INTO sessions(id, project_id, agent_type, attributes) VALUES (1, 1, 'build', '{"manager_id":"owner"}');
		INSERT INTO messages(id, session_id, role, content) VALUES (1, 1, 'user', 'task');
		INSERT INTO session_inbox(session_id, source, raw_content, received_at, delivery_key)
		VALUES (1, 'process', 'done', CURRENT_TIMESTAMP, 'process:1');
		INSERT INTO schedules(session_id, cron_expr, input_message) VALUES (1, '* * * * *', 'tick');
		INSERT INTO session_deliveries(session_id, delivery_id, kind, fingerprint, delivered_at)
		VALUES (1, 'legacy', 'tool_notification', 'fingerprint', CURRENT_TIMESTAMP)`)
	require.NoError(t, err)
	require.Equal(t, 1, countRows(t, db, "session_deliveries", "1 = 1"))
	const schema = `SELECT type, name, tbl_name, sql FROM sqlite_master
		WHERE tbl_name <> 'session_deliveries' ORDER BY type, name`
	schemaBefore := dumpRows(t, db, schema)
	tables := stringColumn(t, db, `SELECT name FROM sqlite_master WHERE type = 'table'
		AND name NOT IN ('session_deliveries', 'goose_db_version', 'sqlite_sequence') ORDER BY name`)
	queries := []string{`SELECT * FROM sqlite_sequence WHERE name <> 'goose_db_version' ORDER BY name`}
	for _, table := range tables {
		queries = append(queries, `SELECT * FROM "`+table+`"`)
	}
	before := make([][]map[string]sql.NullString, len(queries))
	for i, query := range queries {
		before[i] = dumpRows(t, db, query)
	}
	_, err = provider.UpTo(t.Context(), 50)
	require.NoError(t, err)
	assert.False(t, tableExists(t, db, "session_deliveries"))
	assert.Empty(t, dumpRows(t, db, `SELECT name FROM sqlite_master WHERE tbl_name = 'session_deliveries'`))
	assert.Equal(t, schemaBefore, dumpRows(t, db, schema))
	for i, query := range queries {
		assert.Equal(t, before[i], dumpRows(t, db, query), query)
	}
	assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
}
