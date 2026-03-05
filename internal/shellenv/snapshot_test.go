package shellenv

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshot_ReusesWithinTTLThenRecapturesAfterExpiry(t *testing.T) {
	p := fakeProvider(t)
	wd := t.TempDir()

	path1 := p.Snapshot(context.Background(), wd)
	require.NotEmpty(t, path1)
	assert.Equal(t, int64(1), p.captureN.Load())

	// Within TTL: reuse, no re-capture.
	path2 := p.Snapshot(context.Background(), wd)
	assert.Equal(t, path1, path2)
	assert.Equal(t, int64(1), p.captureN.Load(), "fresh snapshot must not re-capture")

	// Age the file beyond TTL → exactly one re-capture.
	old := time.Now().Add(-10 * time.Minute)
	require.NoError(t, os.Chtimes(path1, old, old))

	path3 := p.Snapshot(context.Background(), wd)
	assert.Equal(t, path1, path3)
	assert.Equal(t, int64(2), p.captureN.Load(), "expired snapshot must re-capture once")
}

func TestSnapshot_MissingWorkDirReturnsEmpty(t *testing.T) {
	p := fakeProvider(t)

	assert.Empty(t, p.Snapshot(context.Background(), "/no/such/dir/xyz"))
	assert.Equal(t, int64(0), p.captureN.Load())
}

func TestSnapshot_CaptureFailureReturnsEmpty(t *testing.T) {
	p := fakeProvider(t)
	p.captureFn = func(context.Context, string) ([]byte, error) {
		return nil, errors.New("boom")
	}

	assert.Empty(t, p.Snapshot(context.Background(), t.TempDir()))
	assert.Equal(t, int64(1), p.captureN.Load())
}

func TestSnapshot_FileMode0600(t *testing.T) {
	p := fakeProvider(t)

	path := p.Snapshot(context.Background(), t.TempDir())
	require.NotEmpty(t, path)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSnapshot_ConcurrentFirstSpawnsCaptureOnce(t *testing.T) {
	p := fakeProvider(t)
	p.captureFn = func(context.Context, string) ([]byte, error) {
		time.Sleep(20 * time.Millisecond) // widen the race window
		return []byte("declare -x PATH=\"/bin\"\n"), nil
	}

	wd := t.TempDir()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			p.Snapshot(context.Background(), wd)
		})
	}

	wg.Wait()

	assert.Equal(t, int64(1), p.captureN.Load(), "per-key lock must serialize first-spawns to one capture")
}

func TestSnapshot_DistinctWorkDirsGetDistinctFiles(t *testing.T) {
	p := fakeProvider(t)

	a := p.Snapshot(context.Background(), t.TempDir())
	b := p.Snapshot(context.Background(), t.TempDir())

	require.NotEmpty(t, a)
	require.NotEmpty(t, b)
	assert.NotEqual(t, a, b)
	assert.Equal(t, int64(2), p.captureN.Load())
}

func TestClose_RemovesCacheDir(t *testing.T) {
	p := fakeProvider(t)
	dir := p.cacheDir

	_ = p.Snapshot(context.Background(), t.TempDir())

	require.NoError(t, p.Close())

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "cache dir must be removed on Close")
}
