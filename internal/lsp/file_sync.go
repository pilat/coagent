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

func (c *client) ensureFileOpen(ctx context.Context, file string) error {
	return c.syncFile(ctx, file)
}

func (c *client) syncFile(ctx context.Context, file string) error {
	c.fileMu.Lock()
	defer c.fileMu.Unlock()

	content, err := readRegularFile(file)
	if err != nil {
		return err
	}

	uri := pathToURI(file)

	state, opened := c.files[uri]
	if opened && state.content == string(content) {
		return nil
	}

	version := state.version + 1
	method := "textDocument/didChange"

	params := didChangeParams(uri, content, version)
	if version == 1 {
		method = "textDocument/didOpen"
		params = didOpenParams(uri, file, content)
	}

	if err := c.notify(ctx, method, params); err != nil {
		return err
	}

	c.rememberFile(uri, content, version)

	return nil
}

func (c *client) rememberFile(uri string, content []byte, version int) {
	if c.files == nil {
		c.files = make(map[string]documentState)
	}

	c.files[uri] = documentState{content: string(content), version: version}
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

func didOpenParams(uri, file string, content []byte) map[string]any {
	return map[string]any{
		lspKeyTextDocument: map[string]any{
			lspKeyURI:    uri,
			"languageId": languageID(file),
			"version":    1,
			"text":       string(content),
		},
	}
}

func didChangeParams(uri string, content []byte, version int) map[string]any {
	return map[string]any{
		lspKeyTextDocument: map[string]any{
			lspKeyURI: uri,
			"version": version,
		},
		"contentChanges": []map[string]any{{"text": string(content)}},
	}
}
