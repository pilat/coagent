package lsp

import (
	"context"
	"fmt"
)

func (m *manager) openFile(ctx context.Context, workDir, file string) (*client, fileIdentity, error) {
	identity, err := m.resolveFile(workDir, file)
	if err != nil {
		return nil, fileIdentity{}, err
	}

	cl, err := m.getClient(ctx, workDir, identity.path)
	if err != nil {
		return nil, fileIdentity{}, err
	}

	if _, err := cl.syncFile(ctx, identity.path); err != nil {
		return nil, fileIdentity{}, fmt.Errorf("didOpen: %w", err)
	}

	return cl, identity, nil
}

func positionParams(file string, line, character int) map[string]any {
	return map[string]any{
		lspKeyTextDocument: map[string]any{lspKeyURI: fileURI(file)},
		"position":         map[string]int{"line": line, "character": character},
	}
}

func docParams(file string) map[string]any {
	return map[string]any{
		lspKeyTextDocument: map[string]any{lspKeyURI: fileURI(file)},
	}
}
