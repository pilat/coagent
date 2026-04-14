package lsp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

func (m *manager) GetDiagnostics(ctx context.Context, workDir, file string) ([]Diagnostic, error) {
	log := logger.Ctx(ctx).Named("lsp.manager")

	cl, err := m.getClient(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	if err := cl.ensureFileOpen(ctx, file); err != nil {
		return nil, fmt.Errorf("didOpen: %w", err)
	}

	time.Sleep(100 * time.Millisecond)

	uri := pathToURI(file)
	diags := cl.getDiagnostics(uri)
	log.Info("GetDiagnostics",
		zap.String("file", file),
		zap.String("uri", uri),
		zap.Int("count", len(diags)),
	)

	if len(diags) > 0 {
		log.Debug("GetDiagnostics: errors", zap.Any("diagnostics", diags))
	}

	return diags, nil
}

func (m *manager) Definition(ctx context.Context, workDir, file string, line, character int) ([]Location, error) {
	cl, err := m.getClient(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	if err := cl.ensureFileOpen(ctx, file); err != nil {
		return nil, fmt.Errorf("didOpen: %w", err)
	}

	var result []Location
	if err := cl.call(ctx, "textDocument/definition", positionParams(file, line, character), &result); err != nil {
		return nil, fmt.Errorf("definition: %w", err)
	}

	return result, nil
}

func (m *manager) References(ctx context.Context, workDir, file string, line, character int) ([]Location, error) {
	cl, err := m.getClient(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	if err := cl.ensureFileOpen(ctx, file); err != nil {
		return nil, fmt.Errorf("didOpen: %w", err)
	}

	params := positionParams(file, line, character)
	params["context"] = map[string]bool{"includeDeclaration": true}

	var result []Location
	if err := cl.call(ctx, "textDocument/references", params, &result); err != nil {
		return nil, fmt.Errorf("references: %w", err)
	}

	return result, nil
}

func (m *manager) Hover(ctx context.Context, workDir, file string, line, character int) (*Hover, error) {
	cl, err := m.getClient(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	if err := cl.ensureFileOpen(ctx, file); err != nil {
		return nil, fmt.Errorf("didOpen: %w", err)
	}

	var result Hover
	if err := cl.call(ctx, "textDocument/hover", positionParams(file, line, character), &result); err != nil {
		return nil, fmt.Errorf("hover: %w", err)
	}

	return &result, nil
}

func (m *manager) DocumentSymbol(ctx context.Context, workDir, file string) ([]DocumentSymbol, error) {
	cl, err := m.getClient(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	if err := cl.ensureFileOpen(ctx, file); err != nil {
		return nil, fmt.Errorf("didOpen: %w", err)
	}

	var result []DocumentSymbol
	if err := cl.call(ctx, "textDocument/documentSymbol", docParams(file), &result); err != nil {
		return nil, fmt.Errorf("documentSymbol: %w", err)
	}

	return result, nil
}

func (m *manager) Implementation(ctx context.Context, workDir, file string, line, character int) ([]Location, error) {
	cl, err := m.getClient(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	if err := cl.ensureFileOpen(ctx, file); err != nil {
		return nil, fmt.Errorf("didOpen: %w", err)
	}

	var result []Location
	if err := cl.call(ctx, "textDocument/implementation", positionParams(file, line, character), &result); err != nil {
		return nil, fmt.Errorf("implementation: %w", err)
	}

	return result, nil
}

func (m *manager) PrepareCallHierarchy(
	ctx context.Context,
	workDir, file string,
	line, character int,
) ([]CallHierarchyItem, error) {
	cl, err := m.getClient(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	if err := cl.ensureFileOpen(ctx, file); err != nil {
		return nil, fmt.Errorf("didOpen: %w", err)
	}

	var result []CallHierarchyItem
	if err := cl.call(
		ctx,
		"textDocument/prepareCallHierarchy",
		positionParams(file, line, character),
		&result,
	); err != nil {
		return nil, fmt.Errorf("prepareCallHierarchy: %w", err)
	}

	return result, nil
}

func (m *manager) IncomingCalls(
	ctx context.Context,
	workDir, file string,
	line, character int,
) ([]CallHierarchyIncomingCall, error) {
	cl, err := m.getClient(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	if err := cl.ensureFileOpen(ctx, file); err != nil {
		return nil, fmt.Errorf("didOpen: %w", err)
	}

	var items []CallHierarchyItem
	if err := cl.call(
		ctx,
		"textDocument/prepareCallHierarchy",
		positionParams(file, line, character),
		&items,
	); err != nil {
		return nil, fmt.Errorf("prepareCallHierarchy: %w", err)
	}

	if len(items) == 0 {
		return nil, nil
	}

	var result []CallHierarchyIncomingCall
	if err := cl.call(ctx, "callHierarchy/incomingCalls", map[string]any{"item": items[0]}, &result); err != nil {
		return nil, fmt.Errorf("incomingCalls: %w", err)
	}

	return result, nil
}

func (m *manager) OutgoingCalls(
	ctx context.Context,
	workDir, file string,
	line, character int,
) ([]CallHierarchyOutgoingCall, error) {
	cl, err := m.getClient(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	if err := cl.ensureFileOpen(ctx, file); err != nil {
		return nil, fmt.Errorf("didOpen: %w", err)
	}

	var items []CallHierarchyItem
	if err := cl.call(
		ctx,
		"textDocument/prepareCallHierarchy",
		positionParams(file, line, character),
		&items,
	); err != nil {
		return nil, fmt.Errorf("prepareCallHierarchy: %w", err)
	}

	if len(items) == 0 {
		return nil, nil
	}

	var result []CallHierarchyOutgoingCall
	if err := cl.call(ctx, "callHierarchy/outgoingCalls", map[string]any{"item": items[0]}, &result); err != nil {
		return nil, fmt.Errorf("outgoingCalls: %w", err)
	}

	return result, nil
}

// findClientForWorkDir finds a cached client whose root matches workDir, or falls back to any client.
func (m *manager) findClientForWorkDir(workDir string) *client {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for key, c := range m.clients {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) < 2 {
			continue
		}

		if strings.HasPrefix(parts[1], workDir) {
			return c
		}
	}

	for _, c := range m.clients {
		return c
	}

	return nil
}

func (m *manager) WorkspaceSymbol(ctx context.Context, workDir, query string) ([]SymbolInformation, error) {
	cl := m.findClientForWorkDir(workDir)
	if cl == nil {
		return nil, errors.New("no LSP client available")
	}

	var result []SymbolInformation
	if err := cl.call(ctx, "workspace/symbol", map[string]any{"query": query}, &result); err != nil {
		return nil, fmt.Errorf("workspaceSymbol: %w", err)
	}

	return result, nil
}

func (m *manager) GetAllDiagnostics(ctx context.Context, _ string, maxErrorsPerFile, maxFiles int) []FileDiagnostics {
	log := logger.Ctx(ctx).Named("lsp.manager")

	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []FileDiagnostics
	filesChecked := 0
	totalErrors := 0

	for _, cl := range m.clients {
		allDiags := cl.getAllDiagnostics()
		for uri, diagnostics := range allDiags {
			if filesChecked >= maxFiles {
				break
			}

			var fileErrors []Diagnostic
			for _, d := range diagnostics {
				if d.Severity == 1 && len(fileErrors) < maxErrorsPerFile {
					fileErrors = append(fileErrors, d)
					totalErrors++
				}
			}

			if len(fileErrors) > 0 {
				path := uri
				if strings.HasPrefix(uri, "file://") {
					path = uri[7:]
				}

				result = append(result, FileDiagnostics{
					Path:        path,
					Diagnostics: fileErrors,
				})
				filesChecked++
			}
		}
	}

	log.Info("GetAllDiagnostics",
		zap.Int("filesWithErrors", len(result)),
		zap.Int("totalErrors", totalErrors),
		zap.Int("maxFiles", maxFiles),
	)

	if len(result) > 0 {
		log.Debug("GetAllDiagnostics: files", zap.Any("files", result))
	}

	return result
}

// positionParams builds textDocument/position params for LSP requests.
func positionParams(file string, line, character int) map[string]any {
	return map[string]any{
		lspKeyTextDocument: map[string]any{lspKeyURI: pathToURI(file)},
		"position":         map[string]int{"line": line, "character": character},
	}
}

// docParams builds textDocument params for LSP requests.
func docParams(file string) map[string]any {
	return map[string]any{
		lspKeyTextDocument: map[string]any{lspKeyURI: pathToURI(file)},
	}
}
