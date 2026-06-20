package lsp

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A server that spawns but never answers initialize must fail on lspInitTimeout,
// not hold the start (and, pre-fix, mu) open forever.
func TestManager_StartClient_HandshakeTimesOut(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	restore := lspInitTimeout
	lspInitTimeout = 300 * time.Millisecond

	t.Cleanup(func() { lspInitTimeout = restore })

	m := &manager{
		servers: []serverConfig{{
			ID:         "srv",
			Extensions: []string{".go"},
			RootFinder: func(_, _ string) (string, error) { return "/root", nil },
			Spawn: func(_ context.Context, _ string) (*exec.Cmd, error) {
				return exec.Command("sleep", "60"), nil // spawns but never speaks LSP
			},
		}},
		clients: make(map[string]*client),
	}

	done := make(chan error, 1)
	start := time.Now()

	go func() {
		_, err := m.getClient(context.Background(), "/proj", "/proj/main.go")
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "a server that never answers initialize must fail on the deadline")
		require.Less(t, time.Since(start), 10*time.Second, "must fail on lspInitTimeout, not hang")
	case <-time.After(10 * time.Second):
		t.Fatal("getClient hung on a server that never answers initialize")
	}
}

// A start stuck in Spawn (slow install / handshake) holds only its per-key lock,
// not mu — so Close and a start for a different root must proceed. Pre-fix, mu was
// held across Spawn and both would wedge.
func TestManager_StartClient_SlowSpawnDoesNotBlockCloseOrOtherRoots(t *testing.T) {
	blockA := make(chan struct{})
	enteredA := make(chan struct{})

	t.Cleanup(func() { close(blockA) })

	m := &manager{
		servers: []serverConfig{{
			ID:         "srv",
			Extensions: []string{".go"},
			RootFinder: func(_, file string) (string, error) {
				if strings.Contains(file, "/a/") {
					return "/root/a", nil
				}

				return "/root/b", nil
			},
			Spawn: func(_ context.Context, root string) (*exec.Cmd, error) {
				if root == "/root/a" {
					close(enteredA)
					<-blockA

					return nil, errors.New("released")
				}

				return nil, errors.New("no server for b")
			},
		}},
		clients: make(map[string]*client),
	}

	go func() { _, _ = m.getClient(context.Background(), "/proj", "/proj/a/main.go") }()
	<-enteredA // A is stuck in Spawn, holding only keyLock[A]

	doneClose := make(chan struct{})
	go func() { m.Close(); close(doneClose) }()

	select {
	case <-doneClose:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked behind an in-flight slow spawn")
	}

	doneB := make(chan struct{})
	go func() {
		_, _ = m.getClient(context.Background(), "/proj", "/proj/b/main.go")
		close(doneB)
	}()

	select {
	case <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("getClient for root B blocked behind a slow spawn of root A")
	}
}
