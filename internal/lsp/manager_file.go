package lsp

import (
	"context"
	"fmt"
)

func (m *manager) openFile(ctx context.Context, workDir, file string) (*client, fileIdentity, error) {
	identity, err := resolveFile(workDir, file)
	if err != nil {
		return nil, fileIdentity{}, err
	}

	cl, err := m.getClient(ctx, workDir, identity.path)
	if err != nil {
		return nil, fileIdentity{}, err
	}

	if err := cl.ensureFileOpen(ctx, identity.path); err != nil {
		return nil, fileIdentity{}, fmt.Errorf("didOpen: %w", err)
	}

	return cl, identity, nil
}

func positionParams(file string, line, character int) map[string]any {
	return map[string]any{
		lspKeyTextDocument: map[string]any{lspKeyURI: pathToURI(file)},
		"position":         map[string]int{"line": line, "character": character},
	}
}

func docParams(file string) map[string]any {
	return map[string]any{
		lspKeyTextDocument: map[string]any{lspKeyURI: pathToURI(file)},
	}
}
