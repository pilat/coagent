//go:build unix

package lsp

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// LSP document synchronization reads the target before notifying the server.
// A writer-less FIFO would block that read uncancelably, so the stat gate must
// reject it first.
func TestClient_EnsureFileOpen_FIFORejected(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	c := newClient()

	done := make(chan error, 1)
	go func() { done <- c.ensureFileOpen(context.Background(), fifo) }()

	select {
	case err := <-done:
		require.Error(t, err, "ensureFileOpen on a FIFO must return an error")
	case <-time.After(5 * time.Second):
		t.Fatal("ensureFileOpen hung opening a writer-less FIFO")
	}
}
