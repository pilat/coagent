package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_StopReturnsPromptlyWhenServerMute(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	cmd := exec.Command("sleep", "60")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	c := &client{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		reader:  bufio.NewReader(stdout),
		pending: make(map[int64]chan rpcResult),
	}
	go c.readLoop(context.Background())

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- c.stop(context.Background()) }()
	select {
	case <-done:
		require.Less(t, time.Since(start), 5*time.Second)
	case <-time.After(10 * time.Second):
		t.Fatal("stop hung on a server that never answers shutdown")
	}
}

func TestClient_ReadLoopCleanup(t *testing.T) {
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()
	c := &client{stdin: clientWrite, reader: bufio.NewReader(clientRead), pending: make(map[int64]chan rpcResult)}
	respChan := make(chan rpcResult, 1)
	c.pendingMu.Lock()
	c.pending[999] = respChan
	c.pendingMu.Unlock()
	go c.readLoop(context.Background())
	_ = serverWrite.Close()
	_ = serverRead.Close()

	select {
	case result := <-respChan:
		require.ErrorIs(t, result.err, ErrClientExited)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for client exit")
	}

	c.pendingMu.Lock()
	assert.Empty(t, c.pending)
	c.pendingMu.Unlock()
}

func TestClient_DiagnosticsNotification(t *testing.T) {
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()
	c := &client{stdin: clientWrite, reader: bufio.NewReader(clientRead), pending: make(map[int64]chan rpcResult)}
	go c.readLoop(context.Background())
	t.Cleanup(func() {
		_ = clientWrite.Close()
		_ = serverRead.Close()
		_ = serverWrite.Close()
	})

	sendNotificationToClient(t, serverWrite, "textDocument/publishDiagnostics", PublishDiagnosticsParams{
		URI: "file:///tmp/test.go",
		Diagnostics: []Diagnostic{{
			Range:    Range{Start: Position{Line: 5}, End: Position{Line: 5, Character: 10}},
			Severity: 1,
			Message:  "undefined: foo",
		}},
	})
	require.Eventually(
		t,
		func() bool { return len(c.getDiagnostics("file:///tmp/test.go")) > 0 },
		time.Second,
		10*time.Millisecond,
	)
	assert.Equal(t, "undefined: foo", c.getDiagnostics("file:///tmp/test.go")[0].Message)
}

func TestClient_ConcurrentCalls(t *testing.T) {
	c := newTestClient(t, func(req Request) (any, error) {
		return Hover{Contents: MarkupContent{Kind: "markdown", Value: fmt.Sprintf("response-%d", req.ID)}}, nil
	})

	const count = 20
	var wg sync.WaitGroup
	results := make([]string, count)
	errs := make([]error, count)
	for i := range count {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			var hover Hover
			errs[index] = c.call(context.Background(), "textDocument/hover", nil, &hover)
			results[index] = hover.Contents.Value
		}(i)
	}
	wg.Wait()
	for index, result := range results {
		require.NoError(t, errs[index])
		assert.NotEmpty(t, result)
	}
}

func TestClient_GetAllDiagnostics(t *testing.T) {
	c := newClient()
	c.diagnostics["file:///a.go"] = []Diagnostic{{Message: "error in a", Severity: 1}}
	c.diagnostics["file:///b.go"] = []Diagnostic{
		{Message: "warning in b", Severity: 2},
		{Message: "error in b", Severity: 1},
	}
	all := c.getAllDiagnostics()
	assert.Len(t, all, 2)
	all["file:///a.go"][0].Message = "mutated"
	assert.Equal(t, "error in a", c.getDiagnostics("file:///a.go")[0].Message)
}

func TestClient_GetDiagnosticsUnknownURI(t *testing.T) {
	assert.Nil(t, newClient().getDiagnostics("file:///nonexistent.go"))
}

func TestClient_Notify(t *testing.T) {
	received := make(chan Notification, 1)
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()
	c := &client{stdin: clientWrite, reader: bufio.NewReader(clientRead), pending: make(map[int64]chan rpcResult)}
	go func() {
		reader := bufio.NewReader(serverRead)
		line, _ := reader.ReadString('\n')
		var length int
		_, _ = fmt.Sscanf(line, "Content-Length: %d", &length)
		_, _ = reader.ReadString('\n')
		body := make([]byte, length)
		_, _ = io.ReadFull(reader, body)
		var notification Notification
		_ = json.Unmarshal(body, &notification)
		received <- notification
	}()
	t.Cleanup(func() {
		_ = clientWrite.Close()
		_ = serverRead.Close()
		_ = serverWrite.Close()
		_ = clientRead.Close()
	})

	require.NoError(
		t,
		c.notify(context.Background(), "textDocument/didOpen", map[string]string{"uri": "file:///test.go"}),
	)
	select {
	case notification := <-received:
		assert.JSONEq(t, jsonRPCVersion, notification.JSONRPC)
		assert.Equal(t, "textDocument/didOpen", notification.Method)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification")
	}
}

func FuzzReadLSPFrame(f *testing.F) {
	f.Add([]byte("Content-Length: 2\r\n\r\n{}"))
	f.Add([]byte("Content-Length: 1\n\n{}"))
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = readLSPFrame(bufio.NewReader(bytes.NewReader(data))) })
}
