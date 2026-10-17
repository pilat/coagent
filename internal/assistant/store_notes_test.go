package assistant

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssistantNotesEditsRekeyAndRollback(t *testing.T) {
	t.Parallel()
	_, store, memory, root := assistantFixture(t)
	old := time.Unix(1000, 0)
	require.NoError(t, store.WithTx(t.Context(), func(tx *sql.Tx) error {
		return SaveNotesTx(
			t.Context(),
			tx,
			root,
			map[string]*string{"wait.raw": new("pending"), "open.keep": new("keep")},
			old,
		)
	}))
	require.NoError(t, store.WithTx(t.Context(), func(tx *sql.Tx) error {
		if err := rekeyNoteTx(t.Context(), tx, root, "wait.raw", "wait.123"); err != nil {
			return err
		}
		return SaveNotesTx(
			t.Context(),
			tx,
			root,
			map[string]*string{"open.keep": new("new"), "open.absent": nil},
			old.Add(time.Hour),
		)
	}))
	notes, err := memory.Notes(t.Context(), root)
	require.NoError(t, err)
	assert.Equal(
		t,
		[]Note{{Key: "open.keep", Text: "new", WrittenAt: 4600}, {Key: "wait.123", Text: "pending", WrittenAt: 1000}},
		notes,
	)
	abort := errors.New("abort")
	err = store.WithTx(t.Context(), func(tx *sql.Tx) error {
		require.NoError(t, SaveNotesTx(t.Context(), tx, root, map[string]*string{"open.keep": nil}, time.Now()))
		return abort
	})
	require.ErrorIs(t, err, abort)
	after, err := memory.Notes(t.Context(), root)
	require.NoError(t, err)
	assert.Equal(t, notes, after)
	require.NoError(t, store.WithTx(t.Context(), func(tx *sql.Tx) error {
		return SaveNotesTx(t.Context(), tx, root, map[string]*string{"wait.123": nil}, time.Now())
	}))
	after, err = memory.Notes(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, after, 1)
}
