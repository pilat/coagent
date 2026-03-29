package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenDB_ExplicitTransactionsReserveWriterAtBegin(t *testing.T) {
	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "writer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(2)
	require.NoError(t, Run(t.Context(), db, ""))

	first, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Rollback() })

	type beginResult struct {
		tx  *sql.Tx
		err error
	}
	started := make(chan struct{})
	done := make(chan beginResult, 1)
	go func() {
		close(started)
		tx, beginErr := db.BeginTx(context.Background(), nil)
		done <- beginResult{tx: tx, err: beginErr}
	}()
	<-started

	select {
	case result := <-done:
		if result.tx != nil {
			_ = result.tx.Rollback()
		}
		t.Fatal("a second explicit transaction began before the current writer released its slot")
	case <-time.After(100 * time.Millisecond):
		// Expected: BEGIN IMMEDIATE is waiting under busy_timeout.
	}

	require.NoError(t, first.Rollback())
	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.NotNil(t, result.tx)
		require.NoError(t, result.tx.Rollback())
	case <-time.After(2 * time.Second):
		t.Fatal("waiting writer did not begin after the first transaction rolled back")
	}
}
