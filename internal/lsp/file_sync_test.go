package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type notificationCapture struct {
	mu            sync.Mutex
	notifications []Notification
}

func (c *notificationCapture) Write(data []byte) (int, error) {
	if len(data) == 0 || data[0] != '{' {
		return len(data), nil
	}

	var notification Notification
	if err := json.Unmarshal(data, &notification); err != nil {
		return 0, err
	}

	c.mu.Lock()
	c.notifications = append(c.notifications, notification)
	c.mu.Unlock()

	return len(data), nil
}

func (c *notificationCapture) Close() error { return nil }

func (c *notificationCapture) snapshot() []Notification {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]Notification(nil), c.notifications...)
}

func TestManager_TouchFileAndQueriesSynchronizeOneOpenThenChanges(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(file, []byte("package first\n"), 0o600))

	capture := &notificationCapture{}
	cl := newClient()
	cl.stdin = capture
	manager := &manager{
		servers: []serverConfig{{
			ID:         "test",
			Extensions: []string{".go"},
			RootFinder: func(_, _ string) (string, error) { return dir, nil },
		}},
		clients: map[string]*client{"test:" + dir: cl},
	}

	runConcurrentSyncs(t, func(i int) error {
		if i%2 == 0 {
			return manager.TouchFile(t.Context(), dir, file)
		}

		return cl.ensureFileOpen(t.Context(), file)
	})

	require.NoError(t, os.WriteFile(file, []byte("package second\n"), 0o600))
	runConcurrentSyncs(t, func(i int) error {
		if i%2 == 0 {
			return manager.TouchFile(t.Context(), dir, file)
		}

		return cl.ensureFileOpen(t.Context(), file)
	})

	notifications := capture.snapshot()
	require.Len(t, notifications, 2, "concurrent synchronization must emit one event per file version")
	assert.Equal(t, "textDocument/didOpen", notifications[0].Method)
	assert.Equal(t, "textDocument/didChange", notifications[1].Method)

	var change map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(notifications[1].Params, &change))

	var textDocument struct {
		Version int `json:"version"`
	}
	require.NoError(t, json.Unmarshal(change[lspKeyTextDocument], &textDocument))

	var contentChanges []struct {
		Text string `json:"text"`
	}
	require.NoError(t, json.Unmarshal(change["contentChanges"], &contentChanges))
	assert.Equal(t, 2, textDocument.Version)
	require.Len(t, contentChanges, 1)
	assert.Equal(t, "package second\n", contentChanges[0].Text)
}

func runConcurrentSyncs(t *testing.T, fn func(int) error) {
	t.Helper()

	const workers = 32

	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := range workers {
		go func() {
			defer wg.Done()
			<-start
			errs <- fn(i)
		}()
	}

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
}
