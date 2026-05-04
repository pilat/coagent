package lsp

import (
	"bufio"
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

// newTestClient creates a client wired to a mock LSP server via io.Pipe.
// The handler receives parsed requests and returns (result, error).
// Notifications (ID == 0) are silently consumed unless the handler processes them.
func newTestClient(t *testing.T, handler func(req Request) (any, error)) *client {
	t.Helper()

	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()

	c := &client{
		stdin:       clientWrite,
		reader:      bufio.NewReader(clientRead),
		pending:     make(map[int64]chan *json.RawMessage),
		diagnostics: make(map[string][]Diagnostic),
	}
	go c.readLoop(context.Background())

	go func() {
		defer serverWrite.Close()
		reader := bufio.NewReader(serverRead)

		for {
			var contentLength int

			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				line = line[:len(line)-1]
				if line == "\r" || line == "" {
					break
				}
				_, _ = fmt.Sscanf(line, "Content-Length: %d", &contentLength)
			}

			body := make([]byte, contentLength)
			if _, err := io.ReadFull(reader, body); err != nil {
				return
			}

			var req Request
			if err := json.Unmarshal(body, &req); err != nil {
				return
			}

			// Notifications have ID 0 — don't send a response.
			if req.ID == 0 {
				continue
			}

			result, err := handler(req)

			var resp Response
			resp.JSONRPC = "2.0"
			resp.ID = req.ID
			if err != nil {
				resp.Error = &ResponseError{Code: -1, Message: err.Error()}
			} else {
				raw, _ := json.Marshal(result)
				rawMsg := json.RawMessage(raw)
				resp.Result = &rawMsg
			}

			respBytes, _ := json.Marshal(resp)
			header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(respBytes))
			_, _ = serverWrite.Write([]byte(header))
			_, _ = serverWrite.Write(respBytes)
		}
	}()

	t.Cleanup(func() {
		clientWrite.Close()
		serverRead.Close()
	})

	return c
}

// sendNotificationToClient writes a JSON-RPC notification directly into the client's reader pipe.
// This simulates the server pushing a notification to the client.
func sendNotificationToClient(t *testing.T, w io.Writer, method string, params any) {
	t.Helper()

	paramsBytes, err := json.Marshal(params)
	require.NoError(t, err)

	notif := Notification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  paramsBytes,
	}
	notifBytes, err := json.Marshal(notif)
	require.NoError(t, err)

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(notifBytes))
	_, err = w.Write([]byte(header))
	require.NoError(t, err)
	_, err = w.Write(notifBytes)
	require.NoError(t, err)
}

func TestClient_CallAndResponse(t *testing.T) {
	c := newTestClient(t, func(req Request) (any, error) {
		if req.Method == "textDocument/hover" {
			return Hover{
				Contents: MarkupContent{Kind: "markdown", Value: "func Foo()"},
			}, nil
		}
		return nil, fmt.Errorf("unknown method: %s", req.Method)
	})

	ctx := context.Background()
	var result Hover
	err := c.call(ctx, "textDocument/hover", map[string]string{"key": "val"}, &result)

	require.NoError(t, err)
	assert.Equal(t, "markdown", result.Contents.Kind)
	assert.Equal(t, "func Foo()", result.Contents.Value)
}

func TestClient_CallReturnsError(t *testing.T) {
	c := newTestClient(t, func(req Request) (any, error) {
		return nil, fmt.Errorf("server error")
	})

	ctx := context.Background()
	var result json.RawMessage
	err := c.call(ctx, "textDocument/definition", nil, &result)

	// The client receives a nil response channel value when server returns error.
	assert.Error(t, err)
}

func TestClient_CallContextCancellation(t *testing.T) {
	// Server never responds — block forever.
	block := make(chan struct{})
	c := newTestClient(t, func(req Request) (any, error) {
		<-block
		return nil, nil
	})
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := c.call(ctx, "textDocument/hover", nil, nil)

	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// call must fail on lspCallTimeout even when the caller ctx has no deadline —
// otherwise a wedged server hangs the RPC, the tool, and the loop.
func TestClient_CallInternalTimeout(t *testing.T) {
	restore := lspCallTimeout
	lspCallTimeout = 200 * time.Millisecond

	t.Cleanup(func() { lspCallTimeout = restore })

	block := make(chan struct{})
	c := newTestClient(t, func(_ Request) (any, error) {
		<-block
		return nil, nil
	})

	t.Cleanup(func() { close(block) })

	start := time.Now()
	err := c.call(context.Background(), "textDocument/hover", nil, nil)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 5*time.Second, "must fail on lspCallTimeout, not hang")
}

// stop must return promptly even when the server never answers shutdown: the
// kill-first teardown can't be gated on the graceful RPC. The pre-fix code called
// shutdown under context.Background() and hung here forever.
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
		cmd:         cmd,
		stdin:       stdin,
		stdout:      stdout,
		reader:      bufio.NewReader(stdout),
		pending:     make(map[int64]chan *json.RawMessage),
		files:       make(map[string]documentState),
		diagnostics: make(map[string][]Diagnostic),
	}
	go c.readLoop(context.Background())

	done := make(chan error, 1)
	start := time.Now()

	go func() { done <- c.stop() }()

	select {
	case <-done:
		require.Less(t, time.Since(start), 5*time.Second, "stop must not hang on a mute server")
	case <-time.After(10 * time.Second):
		t.Fatal("stop hung on a server that never answers shutdown")
	}
}

func TestClient_ReadLoopCleanup(t *testing.T) {
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()

	c := &client{
		stdin:       clientWrite,
		reader:      bufio.NewReader(clientRead),
		pending:     make(map[int64]chan *json.RawMessage),
		diagnostics: make(map[string][]Diagnostic),
	}

	// Pre-register a pending call channel before readLoop starts.
	respChan := make(chan *json.RawMessage, 1)
	c.pendingMu.Lock()
	c.pending[999] = respChan
	c.pendingMu.Unlock()

	go c.readLoop(context.Background())

	// Close the server side — readLoop should exit and clean up pending.
	serverWrite.Close()
	serverRead.Close()

	// The pending channel should get closed by cleanupPending.
	select {
	case val, ok := <-respChan:
		assert.False(t, ok, "channel should be closed")
		assert.Nil(t, val)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pending channel to close")
	}

	// Pending map should be empty.
	c.pendingMu.Lock()
	assert.Empty(t, c.pending)
	c.pendingMu.Unlock()

	clientWrite.Close()
}

func TestClient_DiagnosticsNotification(t *testing.T) {
	// We need a client where we control the server write side to push notifications.
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()

	c := &client{
		stdin:       clientWrite,
		reader:      bufio.NewReader(clientRead),
		pending:     make(map[int64]chan *json.RawMessage),
		diagnostics: make(map[string][]Diagnostic),
	}
	go c.readLoop(context.Background())

	t.Cleanup(func() {
		clientWrite.Close()
		serverRead.Close()
		serverWrite.Close()
	})

	// Push a publishDiagnostics notification from "server" to client.
	diagParams := PublishDiagnosticsParams{
		URI: "file:///tmp/test.go",
		Diagnostics: []Diagnostic{
			{
				Range:    Range{Start: Position{Line: 5, Character: 0}, End: Position{Line: 5, Character: 10}},
				Severity: 1,
				Message:  "undefined: foo",
			},
		},
	}
	sendNotificationToClient(t, serverWrite, "textDocument/publishDiagnostics", diagParams)

	// Give readLoop a moment to process.
	require.Eventually(t, func() bool {
		return len(c.getDiagnostics("file:///tmp/test.go")) > 0
	}, 2*time.Second, 10*time.Millisecond)

	diags := c.getDiagnostics("file:///tmp/test.go")
	require.Len(t, diags, 1)
	assert.Equal(t, "undefined: foo", diags[0].Message)
	assert.Equal(t, 1, diags[0].Severity)
}

func TestClient_ConcurrentCalls(t *testing.T) {
	c := newTestClient(t, func(req Request) (any, error) {
		// Echo back the method as the hover value so we can distinguish responses.
		return Hover{
			Contents: MarkupContent{Kind: "markdown", Value: fmt.Sprintf("response-%d", req.ID)},
		}, nil
	})

	ctx := context.Background()
	const numGoroutines = 20

	var wg sync.WaitGroup
	results := make([]string, numGoroutines)
	errors := make([]error, numGoroutines)

	for i := range numGoroutines {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()
			var hover Hover
			errors[idx] = c.call(ctx, "textDocument/hover", nil, &hover)
			results[idx] = hover.Contents.Value
		}(i)
	}

	wg.Wait()

	for i := range numGoroutines {
		require.NoError(t, errors[i], "goroutine %d", i)
		assert.NotEmpty(t, results[i], "goroutine %d should have a result", i)
	}
}

func TestClient_GetAllDiagnostics(t *testing.T) {
	c := newClient()

	c.diagnosticsMu.Lock()
	c.diagnostics["file:///a.go"] = []Diagnostic{
		{Message: "error in a", Severity: 1},
	}
	c.diagnostics["file:///b.go"] = []Diagnostic{
		{Message: "warning in b", Severity: 2},
		{Message: "error in b", Severity: 1},
	}
	c.diagnosticsMu.Unlock()

	all := c.getAllDiagnostics()

	assert.Len(t, all, 2)
	assert.Len(t, all["file:///a.go"], 1)
	assert.Len(t, all["file:///b.go"], 2)

	all["file:///a.go"][0].Message = "mutated"
	assert.Equal(t, "error in a", c.getDiagnostics("file:///a.go")[0].Message, "original should be unmodified")
}

func TestClient_GetDiagnostics_UnknownURI(t *testing.T) {
	c := newClient()

	diags := c.getDiagnostics("file:///nonexistent.go")
	assert.Nil(t, diags)
}

func TestClient_Notify(t *testing.T) {
	received := make(chan Notification, 1)

	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()

	c := &client{
		stdin:       clientWrite,
		reader:      bufio.NewReader(clientRead),
		pending:     make(map[int64]chan *json.RawMessage),
		diagnostics: make(map[string][]Diagnostic),
	}

	// Reader goroutine: parse the notification from the server side of the pipe.
	go func() {
		defer close(received)
		reader := bufio.NewReader(serverRead)
		var contentLength int

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = line[:len(line)-1]
			if line == "\r" || line == "" {
				break
			}
			_, _ = fmt.Sscanf(line, "Content-Length: %d", &contentLength)
		}

		body := make([]byte, contentLength)
		if _, err := io.ReadFull(reader, body); err != nil {
			return
		}

		var notif Notification
		if err := json.Unmarshal(body, &notif); err != nil {
			return
		}

		received <- notif
	}()

	t.Cleanup(func() {
		clientWrite.Close()
		serverRead.Close()
		serverWrite.Close()
		clientRead.Close()
	})

	err := c.notify(context.Background(), "textDocument/didOpen", map[string]string{"uri": "file:///test.go"})
	require.NoError(t, err)

	select {
	case notif := <-received:
		assert.Equal(t, "2.0", notif.JSONRPC)
		assert.Equal(t, "textDocument/didOpen", notif.Method)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification")
	}
}
