package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLock_SecondDaemonFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	first, err := acquire(path)
	require.NoError(t, err)

	_, err = acquire(path)
	require.ErrorIs(t, err, errAlreadyRunning)
	require.NoError(t, first.release())

	second, err := acquire(path)
	require.NoError(t, err)
	require.NoError(t, second.release())
}
