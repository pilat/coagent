package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		pending:     make(map[int64]chan rpcResult),
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

func TestClient_CallPreservesJSONNullResult(t *testing.T) {
	c := newTestClient(t, func(Request) (any, error) { return nil, nil })

	var raw json.RawMessage
	require.NoError(t, c.call(context.Background(), "test/null", nil, &raw))
	assert.Equal(t, "null", string(raw))
}

func TestClient_CallReturnsError(t *testing.T) {
	c := newTestClient(t, func(req Request) (any, error) {
		return nil, fmt.Errorf("server error")
	})

	ctx := context.Background()
	var result json.RawMessage
	err := c.call(ctx, "textDocument/definition", nil, &result)

	// The client receives a nil response channel value when server returns error.
	require.Error(t, err)
	var rpcErr *RPCError
	assert.ErrorAs(t, err, &rpcErr)
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
