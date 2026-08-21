package lsp

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

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

	c.ensureProcessWaiter()
	go c.readLoop(context.WithoutCancel(ctx))

	var result initializeResult
	if err := c.call(ctx, "initialize", initializeParams(root), &result); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	if err := result.validatePositionEncoding(); err != nil {
		return err
	}

	if err := c.notify(ctx, "initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized: %w", err)
	}

	return nil
}

func initializeParams(root string) map[string]any {
	return map[string]any{
		"processId":    nil,
		"rootPath":     root,
		"rootUri":      fileURI(root),
		"capabilities": initializeCapabilities(),
		"workspaceFolders": []map[string]string{{
			"uri":  fileURI(root),
			"name": filepath.Base(root),
		}},
	}
}

type initializeResult struct {
	Capabilities struct {
		PositionEncoding string `json:"positionEncoding"` //nolint:tagliatelle // LSP wire key.
	} `json:"capabilities"`
}

func (r initializeResult) validatePositionEncoding() error {
	if r.Capabilities.PositionEncoding == "" || r.Capabilities.PositionEncoding == "utf-16" {
		return nil
	}

	return fmt.Errorf("unsupported LSP position encoding %q", r.Capabilities.PositionEncoding)
}

func initializeCapabilities() map[string]any {
	return map[string]any{
		"workspace": map[string]any{
			"workspaceFolders": true,
			"symbol":           map[string]bool{lspKeyDynamicRegistration: false},
		},
		lspKeyTextDocument: map[string]any{
			"synchronization": map[string]bool{
				lspKeyDynamicRegistration: false,
			},
			"completion":     map[string]any{lspKeyDynamicRegistration: false},
			"hover":          map[string]any{lspKeyDynamicRegistration: false},
			"references":     map[string]bool{lspKeyDynamicRegistration: false},
			"implementation": map[string]bool{lspKeyDynamicRegistration: false},
			"definition": map[string]any{
				lspKeyDynamicRegistration: false,
				"linkSupport":             true,
			},
			"documentSymbol": map[string]any{
				lspKeyDynamicRegistration:           false,
				"hierarchicalDocumentSymbolSupport": true,
			},
			"callHierarchy": map[string]bool{lspKeyDynamicRegistration: false},
		},
	}
}

func (c *client) waitForProcess() {
	defer c.recoverProcessWaiter()

	_ = c.cmd.Wait()
	close(c.processDone)
	c.cleanupPending()
}

func (c *client) recoverProcessWaiter() {
	if recovered := recover(); recovered != nil {
		logger.Named("lsp.client").
			Error("LSP process waiter panic", zap.Any("recovered", recovered), zap.Stack("stack"))
		close(c.processDone)
		c.cleanupPending()
	}
}

func (c *client) ensureProcessWaiter() {
	if c.cmd == nil {
		return
	}

	c.processOnce.Do(func() {
		c.processDone = make(chan struct{})
		go c.waitForProcess()
	})
}

func pathToURI(file string) string { return fileURI(file) }
