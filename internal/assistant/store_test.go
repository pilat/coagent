package assistant

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestAssistantStoreRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, sessions, memory, root := assistantFixture(t)
	desk, err := memory.LoadDesk(ctx, root)
	require.NoError(t, err)
	assert.Empty(t, desk.Diary)
	assert.Empty(t, desk.Stickers)
	assert.True(t, desk.LastTurnAt.IsZero())
	at := time.Now().UTC()
	want := Desk{
		Diary: map[string]string{"user": "Русский"}, Stickers: []Sticker{{Key: "wait.ci", Text: "waiting", On: "244"}},
		Focus: "migration", LastTurnAt: at, UpdatedAt: at,
	}
	input, err := sessions.Enqueue(
		ctx,
		sessionstore.Input{SessionID: root, Source: sessionstore.InputSourceUser, Content: "hello"},
	)
	require.NoError(t, err)
	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		require.NoError(t, SaveDeskTx(ctx, tx, root, want))
		require.NoError(
			t,
			RecordSendTx(
				ctx,
				tx,
				Send{InputID: input.Input.ID, AssistantSessionID: root, SessionID: root, CreatedAt: at},
			),
		)

		return RecordTurnTx(ctx, tx, Turn{
			SessionID: root, InputID: input.Input.ID, Step: 1,
			Desk: "rendered", Response: "raw", Outcome: "ok", CreatedAt: at,
		})
	}))
	desk, err = memory.LoadDesk(ctx, root)
	require.NoError(t, err)
	assert.Equal(t, want, desk)
	sends, err := memory.UnobservedSends(ctx, root)
	require.NoError(t, err)
	require.Len(t, sends, 1)
	assert.Equal(t, input.Input.ID, sends[0].InputID)
	assert.Equal(t, at, sends[0].CreatedAt)
	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		won, err := MarkObservedTx(ctx, tx, input.Input.ID, at)
		require.NoError(t, err)
		assert.True(t, won)
		won, err = MarkObservedTx(ctx, tx, input.Input.ID, at.Add(time.Second))
		require.NoError(t, err)
		assert.False(t, won)

		return nil
	}))
	var observed time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT observed_at FROM assistant_sends`).Scan(&observed))
	assert.Equal(t, at, observed)
	sends, err = memory.UnobservedSends(ctx, 0)
	require.NoError(t, err)
	assert.Empty(t, sends)
	var rendered, raw, outcome string
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT desk, response, outcome FROM assistant_turns`).Scan(&rendered, &raw, &outcome),
	)
	assert.Equal(t, "rendered", rendered)
	assert.Equal(t, "raw", raw)
	assert.Equal(t, "ok", outcome)
}

func TestAssistantTapeVisibilityAndRollback(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, sessions, memory, root := assistantFixture(t)
	at := time.Now().UTC()
	var ids []int64

	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		for range 5 {
			id, err := AppendTapeTx(ctx, tx, root, Line{At: at, Kind: "user", Text: "hello"})
			require.NoError(t, err)
			ids = append(ids, id)
		}

		return HideTapeTx(ctx, tx, root, [][2]int64{{ids[0], ids[1]}})
	}))
	lines, err := memory.VisibleTape(ctx, root, 2, ids[0])
	require.NoError(t, err)
	require.Len(t, lines, 3)
	assert.Equal(t, []int64{ids[0], ids[3], ids[4]}, []int64{lines[0].ID, lines[1].ID, lines[2].ID})
	assert.Equal(t, at, lines[0].At)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM assistant_tape`).Scan(&count))
	assert.Equal(t, 5, count)
	abort := errors.New("abort step")
	err = sessions.WithTx(ctx, func(tx *sql.Tx) error {
		require.NoError(t, SaveDeskTx(ctx, tx, root, Desk{Focus: "lost", UpdatedAt: at}))
		_, appendErr := AppendTapeTx(ctx, tx, root, Line{At: at, Kind: "say", Text: "lost"})
		require.NoError(t, appendErr)

		return abort
	})
	require.ErrorIs(t, err, abort)
	desk, err := memory.LoadDesk(ctx, root)
	require.NoError(t, err)
	assert.Empty(t, desk.Focus)
	lines, err = memory.VisibleTape(ctx, root, 40, 0)
	require.NoError(t, err)
	assert.Len(t, lines, 3)
}

func TestAssistantStoreOwnershipAndConstraints(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, sessions, memory, root := assistantFixture(t)
	other, err := sessions.CreateSession(ctx, 1, "m", "", nil)
	require.NoError(t, err)
	first, err := sessions.Enqueue(
		ctx,
		sessionstore.Input{SessionID: root, Source: sessionstore.InputSourceUser, Content: "one"},
	)
	require.NoError(t, err)
	second, err := sessions.Enqueue(
		ctx,
		sessionstore.Input{SessionID: other.ID, Source: sessionstore.InputSourceUser, Content: "two"},
	)
	require.NoError(t, err)
	at := time.Now().UTC()
	var otherLine int64

	require.NoError(t, sessions.WithTx(ctx, func(tx *sql.Tx) error {
		for _, in := range []*sessionstore.InboxInput{second.Input, first.Input} {
			require.NoError(
				t,
				RecordSendTx(
					ctx,
					tx,
					Send{InputID: in.ID, AssistantSessionID: root, SessionID: in.SessionID, CreatedAt: at},
				),
			)
		}

		var appendErr error
		otherLine, appendErr = AppendTapeTx(ctx, tx, other.ID, Line{At: at, Kind: "user", Text: "private"})

		return appendErr
	}))
	sends, err := memory.UnobservedSends(ctx, 0)
	require.NoError(t, err)
	require.Len(t, sends, 2)
	assert.Less(t, sends[0].InputID, sends[1].InputID)
	sends, err = memory.UnobservedSends(ctx, root)
	require.NoError(t, err)
	require.Len(t, sends, 1)
	assert.Equal(t, first.Input.ID, sends[0].InputID)
	lines, err := memory.VisibleTape(ctx, root, 40, otherLine)
	require.NoError(t, err)
	assert.Empty(t, lines)

	for _, query := range []string{
		`INSERT INTO assistant_desk (session_id, diary, updated_at) VALUES (1, '[]', CURRENT_TIMESTAMP)`,
		`INSERT INTO assistant_tape (session_id, at, kind, text) VALUES (1, CURRENT_TIMESTAMP, 'invalid', '')`,
		`INSERT INTO assistant_tape (session_id, at, kind, text, hidden) VALUES (1, CURRENT_TIMESTAMP, 'user', '', 2)`,
		`INSERT INTO assistant_turns (session_id, input_id, step, desk, response, outcome, created_at)
			VALUES (1, 1, 1, '', '', 'invalid', CURRENT_TIMESTAMP)`,
	} {
		_, err := db.ExecContext(ctx, query)
		require.ErrorContains(t, err, "CHECK", query)
	}
}

func assistantFixture(t *testing.T) (*sql.DB, *sessionstore.Store, Store, int64) {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "assistant.db")
	db, err := migrate.OpenDB(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, migrate.Run(ctx, db, path))
	sessions := sessionstore.NewStore(db)
	project, err := sessions.GetOrCreateProject(ctx, "/project")
	require.NoError(t, err)
	root, err := sessions.CreateSession(ctx, project, "model", "", nil)
	require.NoError(t, err)

	return db, sessions, New(db), root.ID
}
