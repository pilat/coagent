package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

const (
	lspKeyTextDocument        = "textDocument"
	lspKeyURI                 = "uri"
	lspKeyDynamicRegistration = "dynamicRegistration"
)

// lspCallTimeout bounds every RPC so a wedged server can't hang the tool + loop;
// stopTimeout bounds the best-effort graceful shutdown during teardown. Both are
// vars so tests shrink them.
var (
	lspCallTimeout = 30 * time.Second
	stopTimeout    = 3 * time.Second
)

// client represents an LSP client connection.
// Lock order: fileMu may acquire writeMu through notify; pendingMu and
// diagnosticsMu are never held with either of them.
type client struct {
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stdout        io.ReadCloser
	reader        *bufio.Reader
	pendingMu     sync.Mutex
	writeMu       sync.Mutex
	fileMu        sync.Mutex
	idGen         atomic.Int64
	pending       map[int64]chan *json.RawMessage
	rootPath      string
	files         map[string]documentState // key: URI, last content/version synchronized with the server
	diagnostics   map[string][]Diagnostic  // key: URI
	diagnosticsMu sync.RWMutex
}

func newClient() *client {
	return &client{
		pending:     make(map[int64]chan *json.RawMessage),
		files:       make(map[string]documentState),
		diagnostics: make(map[string][]Diagnostic),
	}
}

// stop stops the LSP client. Kill-first so cmd.Wait can't block on a server that
// ignores the graceful shutdown RPC; the RPC is best-effort and time-bounded, so
// it never gates the kill (mirrors mcp.Client.Close).
func (c *client) stop() error {
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}

	_ = c.cmd.Process.Kill()

	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	_ = c.call(ctx, "shutdown", nil, nil)
	_ = c.notify(ctx, "exit", nil)

	cancel()

	_ = c.cmd.Wait() // the kill above makes this a zombie reap; "signal: killed" is expected

	return nil
}

// call sends a request and waits for response, bounded by lspCallTimeout so a
// wedged server fails on a deadline instead of hanging the tool and the loop.
func (c *client) call(ctx context.Context, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(ctx, lspCallTimeout)
	defer cancel()

	id := c.idGen.Add(1)

	paramsBytes, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal params: %w", err)
	}

	req := Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  paramsBytes,
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	respChan := make(chan *json.RawMessage, 1)

	c.pendingMu.Lock()
	c.pending[id] = respChan
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	if err := c.send(ctx, reqBytes); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	select {
	case resp := <-respChan:
		if resp == nil {
			return errors.New("no response")
		}

		if result != nil {
			if err := json.Unmarshal(*resp, result); err != nil {
				return fmt.Errorf("unmarshal result: %w", err)
			}
		}

		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// notify sends a notification.
func (c *client) notify(ctx context.Context, method string, params any) error {
	paramsBytes, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal params: %w", err)
	}

	notif := Notification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  paramsBytes,
	}

	notifBytes, err := json.Marshal(notif)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	return c.send(ctx, notifBytes)
}

// send sends a message to the server.
func (c *client) send(ctx context.Context, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := c.stdin.Write([]byte(header)); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	if _, err := c.stdin.Write(data); err != nil {
		return fmt.Errorf("write data: %w", err)
	}

	// Log sent message (truncate long payloads)
	var msg json.RawMessage
	if len(data) < 500 && json.Unmarshal(data, &msg) == nil {
		logger.Ctx(ctx).Debug("→ sent", zap.String("payload", string(data)))
	} else {
		logger.Ctx(ctx).Debug("→ sent", zap.Int("size", len(data)))
	}

	return nil
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

// readLoop reads responses and notifications from the server.
func (c *client) readLoop(ctx context.Context) {
	defer c.cleanupPending()

	for {
		var contentLength int

		for {
			line, err := c.reader.ReadString('\n')
			if err != nil {
				logger.Ctx(ctx).Debug("readLoop: exit on read error", zap.Error(err))
				return
			}

			line = line[:len(line)-1] // Remove \n
			if line == "\r" || line == "" {
				break
			}

			if n, _ := fmt.Sscanf(line, "Content-Length: %d", &contentLength); n == 1 {
				continue
			}
		}

		body := make([]byte, contentLength)
		if _, err := io.ReadFull(c.reader, body); err != nil {
			logger.Ctx(ctx).Debug("readLoop: exit on read body error", zap.Error(err))
			return
		}

		logger.Ctx(ctx).Debug("← received", zap.Int("size", len(body)))

		// Try to parse as notification first
		var notif Notification
		if err := json.Unmarshal(body, &notif); err == nil && notif.Method != "" {
			c.handleNotification(ctx, &notif)
			continue
		}

		var resp Response
		if err := json.Unmarshal(body, &resp); err != nil {
			logger.Ctx(ctx).Debug("← parse error", zap.Error(err))
			continue
		}

		c.pendingMu.Lock()
		ch, ok := c.pending[resp.ID]
		c.pendingMu.Unlock()

		if ok {
			logger.Ctx(ctx).Debug("← response", zap.Int64("id", resp.ID), zap.Bool("hasError", resp.Error != nil))

			if resp.Error != nil {
				ch <- nil
			} else {
				ch <- resp.Result
			}
		}
	}
}

// cleanupPending closes all pending channels to unblock in-flight calls.
func (c *client) cleanupPending() {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

// handleNotification handles LSP notifications.
func (c *client) handleNotification(ctx context.Context, notif *Notification) {
	if notif.Method != "textDocument/publishDiagnostics" {
		return
	}

	var params PublishDiagnosticsParams
	if err := json.Unmarshal(notif.Params, &params); err != nil {
		logger.Ctx(ctx).Debug("publishDiagnostics: parse error", zap.Error(err))
		return
	}

	c.diagnosticsMu.Lock()
	oldCount := len(c.diagnostics[params.URI])
	c.diagnostics[params.URI] = params.Diagnostics
	newCount := len(params.Diagnostics)

	c.diagnosticsMu.Unlock()

	logger.Ctx(ctx).Info("publishDiagnostics",
		zap.String("uri", params.URI),
		zap.Int("version", params.Version),
		zap.Int("count", newCount),
		zap.Int("oldCount", oldCount),
	)
	logger.Ctx(ctx).Debug("publishDiagnostics: diagnostics",
		zap.String("uri", params.URI),
		zap.Any("diagnostics", params.Diagnostics),
	)
}

// startWithCommand starts the client with an existing command.
func (c *client) startWithCommand(ctx context.Context, cmd *exec.Cmd, root string) error {
	c.rootPath = root
	c.cmd = cmd

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start server: %w", err)
	}

	c.stdin = stdin
	c.stdout = stdout
	c.reader = bufio.NewReader(stdout)

	// readLoop outlives the bounded handshake ctx; strip cancellation but keep the
	// logger value so a handshake deadline can't kill the response reader.
	go c.readLoop(context.WithoutCancel(ctx))

	initParams := map[string]any{
		"processId": nil,
		"rootPath":  root,
		"rootUri":   "file://" + filepath.ToSlash(root),
		"capabilities": map[string]any{
			lspKeyTextDocument: map[string]any{
				"synchronization": map[string]bool{
					lspKeyDynamicRegistration: false,
					"willSave":                true,
					"willSaveWaitUntil":       true,
					"didSave":                 true,
				},
				"completion": map[string]any{
					lspKeyDynamicRegistration: false,
				},
				"hover": map[string]any{
					lspKeyDynamicRegistration: false,
				},
				"definition": map[string]any{
					lspKeyDynamicRegistration: false,
					"linkSupport":             true,
				},
				"documentSymbol": map[string]any{
					lspKeyDynamicRegistration: false,
				},
			},
		},
		"workspaceFolders": nil,
	}

	var result map[string]any
	if err := c.call(ctx, "initialize", initParams, &result); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	if err := c.notify(ctx, "initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized: %w", err)
	}

	return nil
}

// pathToURI converts a file path to a URI.
func pathToURI(file string) string {
	abs, _ := filepath.Abs(file)
	return "file://" + filepath.ToSlash(abs)
}

// languageID returns the language ID for a file.
func languageID(file string) string {
	ext := strings.ToLower(filepath.Ext(file))
	switch ext {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs":
		return "javascript"
	case ".yaml", ".yml":
		return "yaml"
	case ".rs":
		return "rust"
	case ".py", ".pyi":
		return "python"
	case ".lua":
		return "lua"
	default:
		return ext[1:]
	}
}
