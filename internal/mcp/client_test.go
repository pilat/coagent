package mcp

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// muteCallClient accepts tools/call but never replies (until its ctx is
// cancelled) — models a live server that hangs a request.
type muteCallClient struct {
	mcpclient.MCPClient
}

func (m *muteCallClient) CallTool(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

// blockingMCPClient models mcp-go's stdio Close: its Close() blocks on cmd.Wait()
// until the subprocess is killed — here, until cancelRun closes killed.
type blockingMCPClient struct {
	mcpclient.MCPClient
	killed chan struct{}
}

type exitErrorMCPClient struct{ mcpclient.MCPClient }

func (exitErrorMCPClient) Close() error { return &exec.ExitError{} }

func (b *blockingMCPClient) Close() error {
	<-b.killed

	return nil
}

// A server that spawns but never speaks MCP must fail on the init deadline, not
// wedge NewClient forever. This is the regression guard for the session-resume
// freeze: a workdir whose toolchain activation dropped the interpreter from PATH
// left the stdio child unresponsive, and the deadline-free handshake hung the
// whole session goroutine.
func TestNewClient_UnresponsiveServerTimesOut(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	restore := initTimeout
	initTimeout = 300 * time.Millisecond

	t.Cleanup(func() { initTimeout = restore })

	// sleep ignores stdin and writes nothing → the initialize handshake never
	// gets a reply.
	cfg := ServerConfig{Command: "sleep", Args: []string{"60"}}

	done := make(chan error, 1)
	start := time.Now()

	go func() {
		_, err := NewClient(context.Background(), "hang", cfg, nil, nil)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "an unresponsive server must return an error, not a client")
		require.Less(t, time.Since(start), 10*time.Second, "must fail on the deadline, not hang")
	case <-time.After(10 * time.Second):
		t.Fatal("NewClient hung on an unresponsive MCP server")
	}
}

// CallTool fires every loop iteration; a live server that accepts the call and
// never replies must fail on callTimeout, not hang the session loop forever.
func TestClient_CallToolTimesOut(t *testing.T) {
	restore := callTimeout
	callTimeout = 200 * time.Millisecond

	t.Cleanup(func() { callTimeout = restore })

	c := &Client{client: &muteCallClient{}}

	done := make(chan error, 1)
	start := time.Now()

	go func() {
		_, err := c.CallTool(context.Background(), "x", nil)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "a mute tool call must return an error, not a result")
		require.Less(t, time.Since(start), 5*time.Second, "must fail on callTimeout, not hang")
	case <-time.After(10 * time.Second):
		t.Fatal("CallTool hung on a mute MCP server")
	}
}

// Close must kill the child before the graceful client.Close(); otherwise
// client.Close()'s cmd.Wait() blocks forever on a live-but-mute server (and
// callers hold pool.mu across it, deadlocking the daemon). Reversing the order
// in Close() would hang this test.
func TestClient_CloseKillsBeforeBlockingClose(t *testing.T) {
	killed := make(chan struct{})
	c := &Client{
		client:    &blockingMCPClient{killed: killed},
		cancelRun: func() { close(killed) },
	}

	done := make(chan error, 1)
	go func() { done <- c.Close() }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung — cancelRun not called before the blocking client.Close")
	}
}

func TestClient_CloseTreatsPostCancelProcessExitAsSuccess(t *testing.T) {
	c := &Client{client: exitErrorMCPClient{}, cancelRun: func() {}}
	require.NoError(t, c.Close())
	require.NoError(t, c.Close())
}

func TestClient_CloseTreatsPostCancelContextErrorAsSuccess(t *testing.T) {
	c := &Client{client: errorCloseMCPClient{err: context.Canceled}, cancelRun: func() {}}
	require.NoError(t, c.Close())
	require.NoError(t, c.Close())
}

func TestClient_CloseRetainsNonProcessError(t *testing.T) {
	closeErr := errors.New("close failed")
	c := &Client{client: errorCloseMCPClient{err: closeErr}, cancelRun: func() {}}
	require.ErrorIs(t, c.Close(), closeErr)
	require.ErrorIs(t, c.Close(), closeErr)
}
