package sessionstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileReadStoreUpsertsAndLooksUp(t *testing.T) {
	store, _, projectID := newTestStore(t)
	ctx := context.Background()
	session, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)

	first := FileReadRecord{MtimeUnixNano: 1, Size: 2, Hash: "a"}
	require.NoError(t, store.RecordRead(ctx, session.ID, "/tmp/file", first))
	got, found, err := store.LookupRead(ctx, session.ID, "/tmp/file")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, first, got)

	second := FileReadRecord{MtimeUnixNano: 3, Size: 4, Hash: "b"}
	require.NoError(t, store.RecordRead(ctx, session.ID, "/tmp/file", second))
	got, found, err = store.LookupRead(ctx, session.ID, "/tmp/file")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, second, got)
}

func TestFileReadStoreMissingReturnsNotFound(t *testing.T) {
	store, _, _ := newTestStore(t)
	got, found, err := store.LookupRead(context.Background(), 1, "/tmp/missing")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, FileReadRecord{}, got)
}
