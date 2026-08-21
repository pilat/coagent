package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientAwaitDiagnosticsRequiresCurrentVersion(t *testing.T) {
	c := newClient()
	uri := "file:///workspace/main.go"
	result := make(chan []Diagnostic, 1)
	errs := make(chan error, 1)

	go func() {
		diagnostics, err := c.awaitDiagnostics(context.Background(), documentSync{uri: uri, version: 2, changed: true})
		result <- diagnostics
		errs <- err
	}()

	processed := c.diagSignal
	c.publishDiagnostics(t, uri, intPtr(1), "stale")
	<-processed
	select {
	case <-result:
		t.Fatal("stale diagnostics satisfied current document version")
	default:
	}

	c.publishDiagnostics(t, uri, intPtr(2), "fresh")
	require.NoError(t, <-errs)
	diagnostics := <-result
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "fresh", diagnostics[0].Message)
}

func TestClientAwaitDiagnosticsVersionlessIsPostSyncBestEffort(t *testing.T) {
	c := newClient()
	uri := "file:///workspace/main.go"
	c.publishDiagnostics(t, uri, nil, "before sync")

	checkpoint := c.diagnosticsGeneration(uri)
	result := make(chan []Diagnostic, 1)
	errs := make(chan error, 1)
	go func() {
		diagnostics, err := c.awaitDiagnostics(context.Background(), documentSync{
			uri: uri, version: 2, changed: true, generation: checkpoint,
		})
		result <- diagnostics
		errs <- err
	}()

	c.publishDiagnostics(t, uri, nil, "after sync")
	require.NoError(t, <-errs)
	diagnostics := <-result
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "after sync", diagnostics[0].Message)
}

func TestClientAwaitDiagnosticsPropagatesCallerCancellation(t *testing.T) {
	c := newClient()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.awaitDiagnostics(ctx, documentSync{uri: "file:///workspace/main.go", version: 1, changed: true})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestClientDiagnosticsRejectNullAndImpossibleVersions(t *testing.T) {
	c := newClient()
	uri := "file:///workspace/main.go"
	c.files[uri] = documentState{version: 2}
	c.publishDiagnostics(t, uri, intPtr(2), "current")
	c.publishDiagnostics(t, uri, intPtr(1), "stale")
	c.publishDiagnostics(t, uri, intPtr(3), "newer")

	diagnostics := c.getDiagnostics(uri)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "current", diagnostics[0].Message)
	require.Equal(t, "stale", c.staleDiags[uri].diagnostics[0].Message)

	c.handleNotification(context.Background(), &Notification{
		Method: "textDocument/publishDiagnostics",
		Params: json.RawMessage(`{"uri":"file:///workspace/main.go","version":null,"diagnostics":[]}`),
	})
	diagnostics = c.getDiagnostics(uri)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "current", diagnostics[0].Message)
}

func TestClientSyncFileExcludesVersionlessPublicationObservedBeforeWrite(t *testing.T) {
	workDir := t.TempDir()
	file := filepath.Join(workDir, "main.go")
	require.NoError(t, os.WriteFile(file, []byte("package main\n"), 0o644))

	c := newClient()
	c.stdin = syncWriteCloser{Writer: io.Discard}
	uri := pathToURI(file)
	c.publishDiagnostics(t, uri, nil, "before write")

	document, err := c.syncFile(context.Background(), file)
	require.NoError(t, err)
	_, version, versionlessGeneration, _ := c.diagnosticsSnapshot(uri)
	assert.False(t, version.present)
	assert.LessOrEqual(t, versionlessGeneration, document.generation)
}

func TestClientSyncFileAcceptsPublicationDuringSyncFrameWrite(t *testing.T) {
	workDir := t.TempDir()
	file := filepath.Join(workDir, "main.go")
	require.NoError(t, os.WriteFile(file, []byte("package main\n"), 0o644))

	c := newClient()
	uri := pathToURI(file)
	c.stdin = &syncPublishingWriter{client: c, uri: uri}
	document, err := c.syncFile(context.Background(), file)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	diagnostics, err := c.awaitDiagnostics(ctx, document)
	require.NoError(t, err)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "published during sync", diagnostics[0].Message)
}

func (c *client) publishDiagnostics(t *testing.T, uri string, version *int, message string) {
	t.Helper()
	params, err := json.Marshal(map[string]any{
		"uri":         uri,
		"diagnostics": []Diagnostic{{Message: message}},
	})
	require.NoError(t, err)
	if version != nil {
		params, err = json.Marshal(map[string]any{
			"uri":         uri,
			"version":     *version,
			"diagnostics": []Diagnostic{{Message: message}},
		})
		require.NoError(t, err)
	}
	c.handleNotification(context.Background(), &Notification{Method: "textDocument/publishDiagnostics", Params: params})
}

func intPtr(value int) *int { return &value }

type syncWriteCloser struct{ io.Writer }

func (syncWriteCloser) Close() error { return nil }

type syncPublishingWriter struct {
	client    *client
	uri       string
	published bool
}

func (w *syncPublishingWriter) Close() error { return nil }

func (w *syncPublishingWriter) Write(data []byte) (int, error) {
	if !w.published && bytes.Contains(data, []byte(`"method":"textDocument/didOpen"`)) {
		w.published = true
		params, err := json.Marshal(map[string]any{
			"uri":         w.uri,
			"diagnostics": []Diagnostic{{Message: "published during sync"}},
		})
		if err != nil {
			return 0, err
		}
		w.client.handleNotification(context.Background(), &Notification{
			Method: "textDocument/publishDiagnostics",
			Params: params,
		})
	}

	return len(data), nil
}
