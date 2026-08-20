package lsp

import (
	"context"
	"encoding/json"
	"fmt"
)

func (m *manager) Definition(ctx context.Context, workDir, file string, line, character int) ([]Location, error) {
	cl, identity, err := m.openFile(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	var raw json.RawMessage
	if err := cl.call(
		ctx,
		"textDocument/definition",
		positionParams(identity.path, line, character),
		&raw,
	); err != nil {
		return nil, fmt.Errorf("definition: %w", err)
	}

	return decodeLocations(raw)
}

func (m *manager) References(ctx context.Context, workDir, file string, line, character int) ([]Location, error) {
	cl, identity, err := m.openFile(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	params := positionParams(identity.path, line, character)
	params["context"] = map[string]bool{"includeDeclaration": true}

	var raw json.RawMessage
	if err := cl.call(ctx, "textDocument/references", params, &raw); err != nil {
		return nil, fmt.Errorf("references: %w", err)
	}

	return decodeLocations(raw)
}

func (m *manager) Hover(ctx context.Context, workDir, file string, line, character int) (*Hover, error) {
	cl, identity, err := m.openFile(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	var raw json.RawMessage
	if err := cl.call(ctx, "textDocument/hover", positionParams(identity.path, line, character), &raw); err != nil {
		return nil, fmt.Errorf("hover: %w", err)
	}

	return decodeHover(raw)
}

func (m *manager) DocumentSymbol(ctx context.Context, workDir, file string) ([]DocumentSymbol, error) {
	cl, identity, err := m.openFile(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	var raw json.RawMessage
	if err := cl.call(ctx, "textDocument/documentSymbol", docParams(identity.path), &raw); err != nil {
		return nil, fmt.Errorf("documentSymbol: %w", err)
	}

	return decodeDocumentSymbols(raw)
}

func (m *manager) Implementation(ctx context.Context, workDir, file string, line, character int) ([]Location, error) {
	cl, identity, err := m.openFile(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	var raw json.RawMessage
	if err := cl.call(
		ctx,
		"textDocument/implementation",
		positionParams(identity.path, line, character),
		&raw,
	); err != nil {
		return nil, fmt.Errorf("implementation: %w", err)
	}

	return decodeLocations(raw)
}

func (m *manager) PrepareCallHierarchy(
	ctx context.Context,
	workDir, file string,
	line, character int,
) ([]CallHierarchyItem, error) {
	cl, identity, err := m.openFile(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	var result []CallHierarchyItem
	if err := cl.call(
		ctx,
		"textDocument/prepareCallHierarchy",
		positionParams(identity.path, line, character),
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
	cl, identity, err := m.openFile(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	var items []CallHierarchyItem
	if err := cl.call(
		ctx,
		"textDocument/prepareCallHierarchy",
		positionParams(identity.path, line, character),
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
	cl, identity, err := m.openFile(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	var items []CallHierarchyItem
	if err := cl.call(
		ctx,
		"textDocument/prepareCallHierarchy",
		positionParams(identity.path, line, character),
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

func (m *manager) WorkspaceSymbol(ctx context.Context, workDir, file, query string) ([]SymbolInformation, error) {
	cl, _, err := m.openFile(ctx, workDir, file)
	if err != nil {
		return nil, err
	}

	var raw json.RawMessage
	if err := cl.call(ctx, "workspace/symbol", map[string]any{"query": query}, &raw); err != nil {
		return nil, fmt.Errorf("workspaceSymbol: %w", err)
	}

	return decodeWorkspaceSymbols(raw)
}
