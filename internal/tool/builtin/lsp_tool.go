package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/lsp"
	"github.com/pilat/coagent/internal/tool"
)

const (
	lspOperationDocumentSymbol  = "documentSymbol"
	lspOperationWorkspaceSymbol = "workspaceSymbol"
	lspDescription              = `Interact with Language Server Protocol (LSP) servers to get code intelligence features.

Prefer LSP for a known symbol's definition, references, type, implementations, or callers. Use grep for literal text and glob for file discovery. Read the relevant code to locate the symbol before a position-based request; do not guess its position. Before changing a shared symbol, use references or callers to identify affected code.

Supported operations:
- goToDefinition: Find where a symbol is defined
- findReferences: Find all references to a symbol
- hover: Get hover information (documentation, type info) for a symbol
- documentSymbol: Get all symbols (functions, classes, variables) in a document
- workspaceSymbol: Search symbols in the anchor file's language workspace (requires file_path and query)
- goToImplementation: Find implementations of interfaces or abstract methods
- prepareCallHierarchy: Prepare call hierarchy data for a symbol
- incomingCalls: Find functions that call this function
- outgoingCalls: Find functions called by this function

Required parameters:
- file_path: The file to operate on
- operation: The LSP operation to perform

Optional parameters (required only for position-based operations):
- line: The one-based line number
- character: The one-based UTF-16 code-unit offset

Additional parameters:
- query: Query string (only used by workspaceSymbol operation)

LSP servers must be configured for the file type. If the server or operation is unavailable, use grep and read instead of repeating the failed request. Results are limited to the server's language workspace; an empty result is not proof of absence everywhere.

This tool has no diagnostics operation. The edit and write tools include LSP diagnostics in their results when available. Address relevant errors they report; missing diagnostics do not prove the code is correct or replace required tests.`
)

var _ tool.Tool = (*lspTool)(nil)

type lspTool struct {
	workDir string
	manager lsp.Manager
}

type lspParams struct {
	Operation string `json:"operation"`
	FilePath  string `json:"file_path"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
	Query     string `json:"query,omitempty"`
}

func newLspTool(workDir string, manager lsp.Manager) *lspTool {
	return &lspTool{workDir: workDir, manager: manager}
}

func (t *lspTool) ID() string          { return "lsp" }
func (t *lspTool) ParallelSafe() bool  { return false }
func (t *lspTool) Description() string { return lspDescription }

func (t *lspTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"operation": {
				"type": "string",
				"enum": ["goToDefinition", "findReferences", "hover", "documentSymbol", "workspaceSymbol", "goToImplementation", "prepareCallHierarchy", "incomingCalls", "outgoingCalls"],
				"description": "The LSP operation to perform"
			},
			"file_path": {
				"type": "string",
				"description": "The required file and language-server anchor, including for workspaceSymbol"
			},
			"line": {
				"type": "integer",
				"description": "The line number (1-based). Required except for documentSymbol and workspaceSymbol."
			},
			"character": {
				"type": "integer",
				"description": "The UTF-16 code-unit offset within the line (1-based). Required except for documentSymbol and workspaceSymbol."
			},
			"query": {
				"type": "string",
				"description": "Query string for workspaceSymbol operation"
			}
		},
		"required": ["operation", "file_path"]
	}`)
}

func (t *lspTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p lspParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if t.manager == nil {
		return nil, errors.New("LSP manager not initialized")
	}

	return t.dispatch(ctx, p)
}

func (t *lspTool) dispatch(ctx context.Context, p lspParams) (*tool.Result, error) {
	if p.FilePath == "" {
		return nil, errors.New("LSP file_path is required")
	}

	if !knownLSPOperation(p.Operation) {
		return nil, fmt.Errorf("unknown operation: %s", p.Operation)
	}

	needsPosition := p.Operation != lspOperationDocumentSymbol && p.Operation != lspOperationWorkspaceSymbol
	if needsPosition && (p.Line <= 0 || p.Character <= 0) {
		return nil, errors.New("LSP line and character must be positive")
	}
	// Convert the tool's one-based UTF-16 offset to LSP's zero-based value.
	line := p.Line - 1
	char := p.Character - 1

	switch p.Operation {
	case lspOperationWorkspaceSymbol:
		return t.workspaceSymbol(ctx, p.FilePath, p.Query)
	case "goToImplementation":
		return t.implementation(ctx, p.FilePath, line, char)
	case "prepareCallHierarchy":
		return t.prepareCallHierarchy(ctx, p.FilePath, line, char)
	case "incomingCalls":
		return t.incomingCalls(ctx, p.FilePath, line, char)
	case "outgoingCalls":
		return t.outgoingCalls(ctx, p.FilePath, line, char)
	case "goToDefinition":
		return t.definition(ctx, p.FilePath, line, char)
	case "findReferences":
		return t.references(ctx, p.FilePath, line, char)
	case "hover":
		return t.hover(ctx, p.FilePath, line, char)
	case lspOperationDocumentSymbol:
		return t.documentSymbol(ctx, p.FilePath)
	default:
		return nil, fmt.Errorf("unknown operation: %s", p.Operation)
	}
}

func knownLSPOperation(operation string) bool {
	switch operation {
	case lspOperationWorkspaceSymbol,
		"goToImplementation",
		"prepareCallHierarchy",
		"incomingCalls",
		"outgoingCalls",
		"goToDefinition",
		"findReferences",
		"hover",
		lspOperationDocumentSymbol:
		return true
	default:
		return false
	}
}

func lspJSON(v any) (*tool.Result, error) {
	output, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal LSP result: %w", err)
	}

	return &tool.Result{Output: string(output)}, nil
}

func (t *lspTool) workspaceSymbol(ctx context.Context, filePath, query string) (*tool.Result, error) {
	if query == "" {
		return nil, errors.New("workspaceSymbol requires a query")
	}

	symbols, err := t.manager.WorkspaceSymbol(ctx, t.workDir, filePath, query)
	if err != nil {
		return nil, fmt.Errorf("workspaceSymbol: %w", err)
	}

	if len(symbols) == 0 {
		return &tool.Result{Output: "No symbols found"}, nil
	}

	return lspJSON(symbols)
}

func (t *lspTool) implementation(ctx context.Context, filePath string, line, char int) (*tool.Result, error) {
	locations, err := t.manager.Implementation(ctx, t.workDir, filePath, line, char)
	if err != nil {
		return nil, fmt.Errorf("implementation: %w", err)
	}

	if len(locations) == 0 {
		return &tool.Result{Output: "No implementations found"}, nil
	}

	return lspJSON(locations)
}

func (t *lspTool) prepareCallHierarchy(ctx context.Context, filePath string, line, char int) (*tool.Result, error) {
	items, err := t.manager.PrepareCallHierarchy(ctx, t.workDir, filePath, line, char)
	if err != nil {
		return nil, fmt.Errorf("prepareCallHierarchy: %w", err)
	}

	if len(items) == 0 {
		return &tool.Result{Output: "No call hierarchy items found"}, nil
	}

	return lspJSON(items)
}

func (t *lspTool) incomingCalls(ctx context.Context, filePath string, line, char int) (*tool.Result, error) {
	calls, err := t.manager.IncomingCalls(ctx, t.workDir, filePath, line, char)
	if err != nil {
		return nil, fmt.Errorf("incomingCalls: %w", err)
	}

	if len(calls) == 0 {
		return &tool.Result{Output: "No incoming calls found"}, nil
	}

	return lspJSON(calls)
}

func (t *lspTool) outgoingCalls(ctx context.Context, filePath string, line, char int) (*tool.Result, error) {
	calls, err := t.manager.OutgoingCalls(ctx, t.workDir, filePath, line, char)
	if err != nil {
		return nil, fmt.Errorf("outgoingCalls: %w", err)
	}

	if len(calls) == 0 {
		return &tool.Result{Output: "No outgoing calls found"}, nil
	}

	return lspJSON(calls)
}

func (t *lspTool) definition(ctx context.Context, filePath string, line, char int) (*tool.Result, error) {
	locations, err := t.manager.Definition(ctx, t.workDir, filePath, line, char)
	if err != nil {
		return nil, fmt.Errorf("definition: %w", err)
	}

	if len(locations) == 0 {
		return &tool.Result{Output: "No definition found"}, nil
	}

	return lspJSON(locations)
}

func (t *lspTool) references(ctx context.Context, filePath string, line, char int) (*tool.Result, error) {
	locations, err := t.manager.References(ctx, t.workDir, filePath, line, char)
	if err != nil {
		return nil, fmt.Errorf("references: %w", err)
	}

	if len(locations) == 0 {
		return &tool.Result{Output: "No references found"}, nil
	}

	return lspJSON(locations)
}

func (t *lspTool) hover(ctx context.Context, filePath string, line, char int) (*tool.Result, error) {
	hover, err := t.manager.Hover(ctx, t.workDir, filePath, line, char)
	if err != nil {
		return nil, fmt.Errorf("hover: %w", err)
	}

	if hover == nil {
		return &tool.Result{Output: "No hover information available"}, nil
	}

	return &tool.Result{Output: hover.Contents.Value}, nil
}

func (t *lspTool) documentSymbol(ctx context.Context, filePath string) (*tool.Result, error) {
	symbols, err := t.manager.DocumentSymbol(ctx, t.workDir, filePath)
	if err != nil {
		return nil, fmt.Errorf("documentSymbol: %w", err)
	}

	if len(symbols) == 0 {
		return &tool.Result{Output: "No symbols found"}, nil
	}

	return lspJSON(symbols)
}
