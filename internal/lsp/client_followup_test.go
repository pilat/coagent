package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_SendCancellationFailsBlockedWriter(t *testing.T) {
	serverRead, clientWrite := io.Pipe()
	defer serverRead.Close()

	c := newClient()
	c.stdin = clientWrite
	pending := make(chan rpcResult, 1)
	c.pending[1] = pending

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := c.notify(ctx, "textDocument/didOpen", map[string]string{"uri": "file:///main.go"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Eventually(t, c.hasExited, time.Second, time.Millisecond)

	select {
	case result := <-pending:
		require.ErrorIs(t, result.err, ErrClientExited)
	case <-time.After(time.Second):
		t.Fatal("pending request was not completed on writer cancellation")
	}

	select {
	case <-c.writerDone:
	case <-time.After(time.Second):
		t.Fatal("writer did not exit after its pipe was closed")
	}
}

func TestClientWriterRecoversAndStopsAfterPanic(t *testing.T) {
	c := newClient()
	c.stdin = panicWriteCloser{}
	err := c.notify(context.Background(), "textDocument/didOpen", map[string]string{"uri": "file:///main.go"})
	require.ErrorIs(t, err, ErrClientExited)
	require.Eventually(t, c.hasExited, time.Second, time.Millisecond)

	select {
	case <-c.writerDone:
	case <-time.After(time.Second):
		t.Fatal("recovered writer did not terminate")
	}
}

func TestClientSendCancellationSkipsQueuedFrame(t *testing.T) {
	serverRead, clientWrite := io.Pipe()
	defer serverRead.Close()

	c := newClient()
	c.stdin = clientWrite
	c.ensureWriter()
	var writers sync.WaitGroup
	for range lspWriteQueueSize + 1 {
		writers.Go(func() {
			_ = c.notify(context.Background(), "textDocument/didOpen", map[string]string{"uri": "file:///main.go"})
		})
	}
	require.Eventually(
		t,
		func() bool { return len(c.writer) == lspWriteQueueSize },
		time.Second,
		time.Millisecond,
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(
		t,
		c.notify(ctx, "textDocument/didChange", map[string]string{"uri": "file:///main.go"}),
		context.Canceled,
	)
	assert.False(t, c.hasExited())

	c.failWriter()
	writers.Wait()
}

func TestClientCallCancellationAfterAcknowledgedWriteQueuesCancel(t *testing.T) {
	c := newClient()
	c.stdin = panicWriteCloser{}
	c.writer = make(chan *outbound, 2)
	c.writerDone = make(chan struct{})
	c.writerOnce.Do(func() {})

	written := make(chan struct{})
	release := make(chan struct{})
	canceled := make(chan []byte, 1)
	go func() {
		request := <-c.writer
		request.start()
		request.finish()
		close(written)
		<-release
		request.done <- writeResult{}

		request = <-c.writer
		canceled <- request.data
		request.start()
		request.finish()
		request.done <- writeResult{}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- c.call(ctx, "textDocument/hover", nil, nil) }()

	<-written
	cancel()
	close(release)

	require.ErrorIs(t, <-result, context.Canceled)
	assert.False(t, c.hasExited())
	var notification struct {
		Method string `json:"method"`
		Params struct {
			ID int64 `json:"id"`
		} `json:"params"`
	}
	select {
	case data := <-canceled:
		require.NoError(t, json.Unmarshal(data, &notification))
	case <-time.After(time.Second):
		t.Fatal("cancel notification was not queued")
	}
	assert.Equal(t, "$/cancelRequest", notification.Method)
	assert.Equal(t, int64(1), notification.Params.ID)
}

func TestClient_ServerMalformedRequestWithValidIDGetsInvalidRequest(t *testing.T) {
	serverRead, clientWrite := io.Pipe()
	defer serverRead.Close()
	defer clientWrite.Close()
	c := newClient()
	c.stdin = clientWrite

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.routeFrame(context.Background(), []byte(`{"jsonrpc":"2.0","id":"server-id","method":false}`))
	}()

	body, err := readLSPFrame(bufio.NewReader(serverRead))
	require.NoError(t, err)
	var response struct {
		ID    string   `json:"id"`
		Error RPCError `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &response))
	assert.Equal(t, "server-id", response.ID)
	assert.Equal(t, -32600, response.Error.Code)
	<-done
}

func TestClient_InvalidJSONRPCResponseCompletesPendingCall(t *testing.T) {
	tests := []string{
		`{"id":1,"result":null}`,
		`{"jsonrpc":"1.0","id":1,"result":null}`,
	}

	for _, frame := range tests {
		t.Run(frame, func(t *testing.T) {
			c := newClient()
			pending := make(chan rpcResult, 1)
			c.pending[1] = pending

			c.routeFrame(context.Background(), []byte(frame))

			result := <-pending
			assert.ErrorIs(t, result.err, errLSPProtocol)
		})
	}
}

func TestClient_MalformedRPCErrorCompletesPendingCallWithProtocolError(t *testing.T) {
	tests := []string{
		`null`,
		`{}`,
		`{"message":"failed"}`,
		`{"code":-32001}`,
		`{"code":"-32001","message":"failed"}`,
		`{"code":-32001,"message":false}`,
	}
	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			c := newClient()
			pending := make(chan rpcResult, 1)
			c.pending[1] = pending

			c.routeFrame(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"error":`+value+`}`))

			result := <-pending
			assert.ErrorIs(t, result.err, errLSPProtocol)
		})
	}
}

func TestClient_ValidRPCErrorPreservesData(t *testing.T) {
	c := newClient()
	pending := make(chan rpcResult, 1)
	c.pending[1] = pending

	c.routeFrame(context.Background(), []byte(
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"failed","data":{"reason":"test"}}}`,
	))

	result := <-pending
	var rpcErr *RPCError
	require.ErrorAs(t, result.err, &rpcErr)
	assert.Equal(t, "lsp error -32001: failed", rpcErr.Error())
	assert.JSONEq(t, `{"reason":"test"}`, string(rpcErr.Data))
}

func TestClient_InvalidJSONRPCRequestWithValidIDGetsInvalidRequest(t *testing.T) {
	serverRead, clientWrite := io.Pipe()
	defer serverRead.Close()
	defer clientWrite.Close()
	c := newClient()
	c.stdin = clientWrite

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.routeFrame(
			context.Background(),
			[]byte(`{"jsonrpc":"1.0","id":"server-id","method":"workspace/configuration"}`),
		)
	}()

	body, err := readLSPFrame(bufio.NewReader(serverRead))
	require.NoError(t, err)
	var response struct {
		ID    string   `json:"id"`
		Error RPCError `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &response))
	assert.Equal(t, "server-id", response.ID)
	assert.Equal(t, -32600, response.Error.Code)
	<-done
}

func TestInitializeResultPositionEncoding(t *testing.T) {
	assert.NoError(t, (initializeResult{}).validatePositionEncoding())
	assert.NoError(t, initializeResultWithEncoding("utf-16").validatePositionEncoding())
	assert.Error(t, initializeResultWithEncoding("utf-8").validatePositionEncoding())
}

func TestInitializeParamsIncludesRootWorkspaceFolder(t *testing.T) {
	params := initializeParams("/workspace")
	folders, ok := params["workspaceFolders"].([]map[string]string)
	require.True(t, ok)
	require.Equal(t, []map[string]string{{"uri": "file:///workspace", "name": "workspace"}}, folders)
}

func TestInitializeCapabilitiesOmitUnsupportedSaveNotifications(t *testing.T) {
	capabilities := initializeCapabilities()
	textDocument, ok := capabilities[lspKeyTextDocument].(map[string]any)
	require.True(t, ok)
	synchronization, ok := textDocument["synchronization"].(map[string]bool)
	require.True(t, ok)
	assert.Equal(t, map[string]bool{lspKeyDynamicRegistration: false}, synchronization)
}

type panicWriteCloser struct{}

func (panicWriteCloser) Close() error              { return nil }
func (panicWriteCloser) Write([]byte) (int, error) { panic("writer failure") }

func initializeResultWithEncoding(encoding string) initializeResult {
	result := initializeResult{}
	result.Capabilities.PositionEncoding = encoding
	return result
}
