package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

const (
	jsonRPCVersion            = "2.0"
	lspKeyTextDocument        = "textDocument"
	lspKeyURI                 = "uri"
	lspKeyVersion             = "version"
	lspKeyDynamicRegistration = "dynamicRegistration"
	lspWriteQueueSize         = 64
)

// lspCallTimeout bounds every RPC so a wedged server can't hang the tool + loop;
// stopTimeout bounds the best-effort graceful shutdown during teardown. Both are
// vars so tests shrink them.
var (
	lspCallTimeout = 30 * time.Second
	stopTimeout    = 3 * time.Second
)

// client represents one server connection. The process waiter is its only
// reaper; readers and callers only mark the connection unusable.
type client struct {
	cmd         *exec.Cmd
	processDone chan struct{}
	stdin       io.WriteCloser
	stdout      io.ReadCloser
	reader      *bufio.Reader
	pendingMu   sync.Mutex
	writerOnce  sync.Once
	writerStop  sync.Once
	writer      chan *outbound
	writerDone  chan struct{}
	// Lock order is fileMu -> diagnosticsMu -> pendingMu. Writer state is
	// channel-owned; atomic flags only publish irreversible client exit.
	fileMu         sync.Mutex
	idGen          atomic.Int64
	pending        map[int64]chan rpcResult
	rootPath       string
	languageID     string
	files          map[string]documentState // key: URI, last content/version synchronized with the server
	syncing        map[string]chan struct{}
	diagnostics    map[string][]Diagnostic // key: URI
	diagVersions   map[string]diagnosticVersion
	staleDiags     map[string]diagnosticObservation
	diagnosticsMu  sync.RWMutex
	diagnosticGen  map[string]uint64
	versionlessGen map[string]uint64
	diagSignal     chan struct{}
	onExit         func()
	exitOnce       sync.Once
	processOnce    sync.Once
	exited         atomic.Bool
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

func newClient() *client {
	return &client{
		pending:        make(map[int64]chan rpcResult),
		files:          make(map[string]documentState),
		syncing:        make(map[string]chan struct{}),
		diagnostics:    make(map[string][]Diagnostic),
		diagVersions:   make(map[string]diagnosticVersion),
		staleDiags:     make(map[string]diagnosticObservation),
		diagnosticGen:  make(map[string]uint64),
		versionlessGen: make(map[string]uint64),
		diagSignal:     make(chan struct{}),
	}
}

// stop gives a responsive server the LSP shutdown/exit handshake before using
// kill as the bounded fallback for a mute process.
func (c *client) stop(parent context.Context) error {
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}

	c.ensureProcessWaiter()

	deadline := time.NewTimer(stopTimeout)
	defer deadline.Stop()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), stopTimeout)
	_ = c.call(ctx, "shutdown", nil, nil)
	_ = c.notify(ctx, "exit", nil)

	cancel()

	select {
	case <-c.processDone:
	case <-deadline.C:
		_ = c.cmd.Process.Kill()

		<-c.processDone
	}

	return nil
}

// notify sends a notification.
func (c *client) notify(ctx context.Context, method string, params any) error {
	data, err := notificationFrame(method, params)
	if err != nil {
		return err
	}

	return c.send(ctx, data)
}

// send queues one complete JSON-RPC frame. A blocked server stdin cannot hold a
// caller after its context is cancelled because waiting happens outside writer.
func (c *client) send(ctx context.Context, data []byte) error {
	_, err := c.sendFrame(ctx, data, nil)
	return err
}

func (c *client) sendFrame(ctx context.Context, data []byte, beforeWrite func() uint64) (uint64, error) {
	if c.stdin == nil || c.hasExited() {
		return 0, ErrClientExited
	}

	c.ensureWriter()

	request := &outbound{data: append([]byte(nil), data...), done: make(chan writeResult, 1), beforeWrite: beforeWrite}
	select {
	case c.writer <- request:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-c.writerDone:
		return 0, ErrClientExited
	}

	select {
	case result := <-request.done:
		return result.generation, result.err
	case <-ctx.Done():
		if request.cancel() {
			return 0, ctx.Err()
		}

		if request.state.Load() == outboundComplete {
			result := <-request.done
			return result.generation, result.err
		}

		if request.state.Load() == outboundActive {
			c.failWriter()
		}

		return 0, ctx.Err()
	case <-c.writerDone:
		return 0, ErrClientExited
	}
}

func notificationFrame(method string, params any) ([]byte, error) {
	paramsBytes, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}

	data, err := json.Marshal(Notification{JSONRPC: jsonRPCVersion, Method: method, Params: paramsBytes})
	if err != nil {
		return nil, fmt.Errorf("marshal notification: %w", err)
	}

	return data, nil
}

// getDiagnostics returns diagnostics for a URI.
func (c *client) getDiagnostics(uri string) []Diagnostic {
	c.diagnosticsMu.RLock()
	defer c.diagnosticsMu.RUnlock()

	diags, ok := c.diagnostics[uri]
	if !ok {
		return nil
	}

	result := make([]Diagnostic, len(diags))
	copy(result, diags)

	return result
}

// getAllDiagnostics returns a copy of all diagnostics.
func (c *client) getAllDiagnostics() map[string][]Diagnostic {
	c.diagnosticsMu.RLock()
	defer c.diagnosticsMu.RUnlock()

	result := make(map[string][]Diagnostic, len(c.diagnostics))

	for uri, diags := range c.diagnostics {
		cp := make([]Diagnostic, len(diags))
		copy(cp, diags)
		result[uri] = cp
	}

	return result
}

func (c *client) ensureWriter() {
	c.writerOnce.Do(func() {
		c.writer = make(chan *outbound, lspWriteQueueSize)

		c.writerDone = make(chan struct{})

		if c.hasExited() {
			close(c.writerDone)
			return
		}
		go c.writeLoop()
	})
}
