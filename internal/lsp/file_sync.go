package lsp

import (
	"context"
	"fmt"
	"os"
)

type documentState struct {
	content string
	version int
}

type documentSync struct {
	uri        string
	version    int
	changed    bool
	generation uint64
}

func (c *client) ensureFileOpen(ctx context.Context, file string) error {
	_, err := c.syncFile(ctx, file)

	return err
}

func (c *client) syncFile(ctx context.Context, file string) (documentSync, error) {
	content, err := readRegularFile(file)
	if err != nil {
		return documentSync{}, err
	}

	uri := pathToURI(file)

	state, opened, release, err := c.beginFileSync(ctx, uri)
	if err != nil {
		return documentSync{}, err
	}
	defer release()

	if opened && state.content == string(content) {
		return documentSync{uri: uri, version: state.version}, nil
	}

	version := state.version + 1
	method := "textDocument/didChange"

	params := didChangeParams(uri, content, version)
	if version == 1 {
		method = "textDocument/didOpen"
		params = didOpenParams(uri, c.languageID, content)
	}

	c.rememberFile(uri, content, version)

	generation, err := c.notifySync(ctx, method, params, uri)
	if err != nil {
		c.restoreFile(uri, state, opened)
		return documentSync{}, err
	}

	return documentSync{uri: uri, version: version, changed: true, generation: generation}, nil
}

func (c *client) notifySync(ctx context.Context, method string, params any, uri string) (uint64, error) {
	data, err := notificationFrame(method, params)
	if err != nil {
		return 0, err
	}

	// The writer captures this immediately before the actual pipe write. It
	// therefore orders versionless publications against the sync frame itself.
	return c.sendFrame(ctx, data, func() uint64 { return c.diagnosticsGeneration(uri) })
}

func (c *client) restoreFile(uri string, state documentState, opened bool) {
	c.fileMu.Lock()
	defer c.fileMu.Unlock()

	if opened {
		c.files[uri] = state
		return
	}

	delete(c.files, uri)
}

func (c *client) rememberFile(uri string, content []byte, version int) {
	c.fileMu.Lock()
	defer c.fileMu.Unlock()

	if c.files == nil {
		c.files = make(map[string]documentState)
	}

	c.files[uri] = documentState{content: string(content), version: version}
}

func (c *client) beginFileSync(
	ctx context.Context,
	uri string,
) (documentState, bool, func(), error) {
	for {
		c.fileMu.Lock()
		if active := c.syncing[uri]; active != nil {
			c.fileMu.Unlock()

			select {
			case <-active:
				continue
			case <-ctx.Done():
				return documentState{}, false, nil, ctx.Err()
			}
		}

		if c.syncing == nil {
			c.syncing = make(map[string]chan struct{})
		}

		active := make(chan struct{})
		c.syncing[uri] = active
		state, opened := c.files[uri]
		c.fileMu.Unlock()

		return state, opened, func() { c.finishFileSync(uri, active) }, nil
	}
}

func (c *client) finishFileSync(uri string, active chan struct{}) {
	c.fileMu.Lock()
	if c.syncing[uri] == active {
		delete(c.syncing, uri)
		close(active)
	}
	c.fileMu.Unlock()
}

func readRegularFile(file string) ([]byte, error) {
	info, err := os.Stat(file)
	if err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", file)
	}

	content, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	return content, nil
}

func (c *client) diagnosticsGeneration(uri string) uint64 {
	c.diagnosticsMu.RLock()
	defer c.diagnosticsMu.RUnlock()

	return c.diagnosticGen[uri]
}

func didOpenParams(uri, languageID string, content []byte) map[string]any {
	return map[string]any{
		lspKeyTextDocument: map[string]any{
			lspKeyURI:     uri,
			"languageId":  languageID,
			lspKeyVersion: 1,
			"text":        string(content),
		},
	}
}

func didChangeParams(uri string, content []byte, version int) map[string]any {
	return map[string]any{
		lspKeyTextDocument: map[string]any{
			lspKeyURI:     uri,
			lspKeyVersion: version,
		},
		"contentChanges": []map[string]any{{"text": string(content)}},
	}
}
