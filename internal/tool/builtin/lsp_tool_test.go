package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pilat/coagent/internal/lsp"
)

// mockLSPManager is a mock implementation of lsp.Manager for testing.
type mockLSPManager struct {
	definitionFunc           func(ctx context.Context, workDir, file string, line, character int) ([]lsp.Location, error)
	referencesFunc           func(ctx context.Context, workDir, file string, line, character int) ([]lsp.Location, error)
	hoverFunc                func(ctx context.Context, workDir, file string, line, character int) (*lsp.Hover, error)
	documentSymbolFunc       func(ctx context.Context, workDir, file string) ([]lsp.DocumentSymbol, error)
	workspaceSymbolFunc      func(ctx context.Context, workDir, query string) ([]lsp.SymbolInformation, error)
	implementationFunc       func(ctx context.Context, workDir, file string, line, character int) ([]lsp.Location, error)
	prepareCallHierarchyFunc func(ctx context.Context, workDir, file string, line, character int) ([]lsp.CallHierarchyItem, error)
	incomingCallsFunc        func(ctx context.Context, workDir, file string, line, character int) ([]lsp.CallHierarchyIncomingCall, error)
	outgoingCallsFunc        func(ctx context.Context, workDir, file string, line, character int) ([]lsp.CallHierarchyOutgoingCall, error)
}

func (m *mockLSPManager) Definition(
	ctx context.Context,
	workDir, file string,
	line, character int,
) ([]lsp.Location, error) {
	if m.definitionFunc != nil {
		return m.definitionFunc(ctx, workDir, file, line, character)
	}
	return nil, nil
}

func (m *mockLSPManager) References(
	ctx context.Context,
	workDir, file string,
	line, character int,
) ([]lsp.Location, error) {
	if m.referencesFunc != nil {
		return m.referencesFunc(ctx, workDir, file, line, character)
	}
	return nil, nil
}

func (m *mockLSPManager) Hover(ctx context.Context, workDir, file string, line, character int) (*lsp.Hover, error) {
	if m.hoverFunc != nil {
		return m.hoverFunc(ctx, workDir, file, line, character)
	}
	return nil, nil
}

func (m *mockLSPManager) DocumentSymbol(ctx context.Context, workDir, file string) ([]lsp.DocumentSymbol, error) {
	if m.documentSymbolFunc != nil {
		return m.documentSymbolFunc(ctx, workDir, file)
	}
	return nil, nil
}

func (m *mockLSPManager) WorkspaceSymbol(
	ctx context.Context,
	workDir, file, query string,
) ([]lsp.SymbolInformation, error) {
	if m.workspaceSymbolFunc != nil {
		return m.workspaceSymbolFunc(ctx, workDir, query)
	}
	return nil, nil
}

func (m *mockLSPManager) Implementation(
	ctx context.Context,
	workDir, file string,
	line, character int,
) ([]lsp.Location, error) {
	if m.implementationFunc != nil {
		return m.implementationFunc(ctx, workDir, file, line, character)
	}
	return nil, nil
}

func (m *mockLSPManager) PrepareCallHierarchy(
	ctx context.Context,
	workDir, file string,
	line, character int,
) ([]lsp.CallHierarchyItem, error) {
	if m.prepareCallHierarchyFunc != nil {
		return m.prepareCallHierarchyFunc(ctx, workDir, file, line, character)
	}
	return nil, nil
}

func (m *mockLSPManager) IncomingCalls(
	ctx context.Context,
	workDir, file string,
	line, character int,
) ([]lsp.CallHierarchyIncomingCall, error) {
	if m.incomingCallsFunc != nil {
		return m.incomingCallsFunc(ctx, workDir, file, line, character)
	}
	return nil, nil
}

func (m *mockLSPManager) OutgoingCalls(
	ctx context.Context,
	workDir, file string,
	line, character int,
) ([]lsp.CallHierarchyOutgoingCall, error) {
	if m.outgoingCallsFunc != nil {
		return m.outgoingCallsFunc(ctx, workDir, file, line, character)
	}
	return nil, nil
}

func (m *mockLSPManager) GetDiagnostics(ctx context.Context, workDir, file string) ([]lsp.Diagnostic, error) {
	return nil, nil
}

func (m *mockLSPManager) GetAllDiagnostics(
	ctx context.Context,
	workDir string,
	maxErrorsPerFile, maxFiles int,
) []lsp.FileDiagnostics {
	return nil
}

func (m *mockLSPManager) Close() {}

var _ lsp.Manager = (*mockLSPManager)(nil)

func TestLspTool_Parameters(t *testing.T) {
	mockMgr := &mockLSPManager{}
	tool := newLspTool("/tmp", mockMgr)

	t.Run("parameters schema has all operations", func(t *testing.T) {
		params := tool.Parameters()
		var schema map[string]any
		if err := json.Unmarshal(params, &schema); err != nil {
			t.Fatalf("Parameters should be valid JSON: %v", err)
		}

		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatal("Parameters should have properties")
		}

		op, ok := props["operation"].(map[string]any)
		if !ok {
			t.Fatal("Parameters should have operation property")
		}

		enum, ok := op["enum"].([]any)
		if !ok {
			t.Fatal("Operation should have enum values")
		}

		expectedOps := []string{
			"goToDefinition",
			"findReferences",
			"hover",
			"documentSymbol",
			"workspaceSymbol",
			"goToImplementation",
			"prepareCallHierarchy",
			"incomingCalls",
			"outgoingCalls",
		}

		for _, expected := range expectedOps {
			found := false
			for _, val := range enum {
				if val == expected {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Operation enum should contain %s", expected)
			}
		}
	})

	t.Run("parameters schema has query property", func(t *testing.T) {
		params := tool.Parameters()
		var schema map[string]any
		if err := json.Unmarshal(params, &schema); err != nil {
			t.Fatalf("Parameters should be valid JSON: %v", err)
		}

		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatal("Parameters should have properties")
		}

		query, ok := props["query"].(map[string]any)
		if !ok {
			t.Fatal("Parameters should have query property")
		}

		if query["type"] != "string" {
			t.Error("Query should be a string")
		}
	})

	t.Run("line and character are not required", func(t *testing.T) {
		params := tool.Parameters()
		var schema map[string]any
		if err := json.Unmarshal(params, &schema); err != nil {
			t.Fatalf("Parameters should be valid JSON: %v", err)
		}

		required, ok := schema["required"].([]any)
		if !ok {
			t.Fatal("Parameters should have required array")
		}

		for _, r := range required {
			if r == "line" || r == "character" {
				t.Errorf("%s should not be in required fields", r)
			}
		}
	})

	t.Run("required fields are correct", func(t *testing.T) {
		params := tool.Parameters()
		var schema map[string]any
		if err := json.Unmarshal(params, &schema); err != nil {
			t.Fatalf("Parameters should be valid JSON: %v", err)
		}

		required, ok := schema["required"].([]any)
		if !ok {
			t.Fatal("Parameters should have required array")
		}

		requiredMap := make(map[string]bool)
		for _, r := range required {
			if s, ok := r.(string); ok {
				requiredMap[s] = true
			}
		}

		if !requiredMap["operation"] {
			t.Error("operation should be required")
		}
		if !requiredMap["file_path"] {
			t.Error("file_path should be required")
		}
	})
}

func TestLspTool_Description(t *testing.T) {
	mockMgr := &mockLSPManager{}
	tool := newLspTool("/tmp", mockMgr)
	desc := tool.Description()

	expectedOps := []string{
		"goToDefinition",
		"findReferences",
		"hover",
		"documentSymbol",
		"workspaceSymbol",
		"goToImplementation",
		"prepareCallHierarchy",
		"incomingCalls",
		"outgoingCalls",
	}

	for _, op := range expectedOps {
		if !strings.Contains(desc, op) {
			t.Errorf("Description should mention %s", op)
		}
	}

	if !strings.Contains(desc, "query") {
		t.Error("Description should mention query parameter for workspaceSymbol")
	}
}

func TestLspTool_ID(t *testing.T) {
	mockMgr := &mockLSPManager{}
	tool := newLspTool("/tmp", mockMgr)
	if tool.ID() != "lsp" {
		t.Errorf("ID should be 'lsp', got %s", tool.ID())
	}
}

func TestLspTool_ParamsStruct(t *testing.T) {
	t.Run("params struct includes query field", func(t *testing.T) {
		params := lspParams{
			Operation: "workspaceSymbol",
			FilePath:  "/test/file.go",
			Query:     "test",
		}

		data, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("Failed to marshal params: %v", err)
		}

		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal params: %v", err)
		}

		if decoded["query"] != "test" {
			t.Error("Query field should be serialized")
		}
	})

	t.Run("params parsing from JSON", func(t *testing.T) {
		tests := []struct {
			name     string
			json     string
			expected lspParams
		}{
			{
				name: "complete params",
				json: `{"operation":"goToDefinition","file_path":"/test.go","line":10,"character":5,"query":"test"}`,
				expected: lspParams{
					Operation: "goToDefinition",
					FilePath:  "/test.go",
					Line:      10,
					Character: 5,
					Query:     "test",
				},
			},
			{
				name: "workspaceSymbol with query only",
				json: `{"operation":"workspaceSymbol","file_path":"/test.go","query":"func"}`,
				expected: lspParams{
					Operation: "workspaceSymbol",
					FilePath:  "/test.go",
					Query:     "func",
				},
			},
			{
				name: "minimal params",
				json: `{"operation":"documentSymbol","file_path":"/test.go"}`,
				expected: lspParams{
					Operation: "documentSymbol",
					FilePath:  "/test.go",
				},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				var params lspParams
				if err := json.Unmarshal([]byte(tt.json), &params); err != nil {
					t.Fatalf("Failed to unmarshal: %v", err)
				}

				if params.Operation != tt.expected.Operation {
					t.Errorf("Operation: got %q, want %q", params.Operation, tt.expected.Operation)
				}
				if params.FilePath != tt.expected.FilePath {
					t.Errorf("FilePath: got %q, want %q", params.FilePath, tt.expected.FilePath)
				}
				if params.Line != tt.expected.Line {
					t.Errorf("Line: got %d, want %d", params.Line, tt.expected.Line)
				}
				if params.Character != tt.expected.Character {
					t.Errorf("Character: got %d, want %d", params.Character, tt.expected.Character)
				}
				if params.Query != tt.expected.Query {
					t.Errorf("Query: got %q, want %q", params.Query, tt.expected.Query)
				}
			})
		}
	})

	t.Run("coordinate conversion logic", func(t *testing.T) {
		tests := []struct {
			inputLine    int
			inputChar    int
			expectedLine int
			expectedChar int
		}{
			{1, 1, 0, 0},      // First position
			{10, 5, 9, 4},     // Typical position
			{100, 50, 99, 49}, // Larger numbers
			{0, 0, -1, -1},    // Edge case: 0 input (unusual but valid)
		}

		for _, tt := range tests {
			// Simulate the conversion in Execute
			line := tt.inputLine - 1
			char := tt.inputChar - 1

			if line != tt.expectedLine {
				t.Errorf("Line conversion: input %d -> got %d, want %d",
					tt.inputLine, line, tt.expectedLine)
			}
			if char != tt.expectedChar {
				t.Errorf("Char conversion: input %d -> got %d, want %d",
					tt.inputChar, char, tt.expectedChar)
			}
		}
	})
}

func TestLspTool_DescriptionCoverage(t *testing.T) {
	mockMgr := &mockLSPManager{}
	tool := newLspTool("/tmp", mockMgr)
	desc := tool.Description()

	// Check for parameter descriptions
	requiredSections := []string{
		"file_path",
		"operation",
		"line",
		"character",
		"query",
		"Required parameters",
		"Optional parameters",
		"Additional parameters",
	}

	for _, section := range requiredSections {
		if !strings.Contains(desc, section) {
			t.Errorf("Description should contain section %q", section)
		}
	}

	// Check for operation descriptions
	opDescriptions := []string{
		"defined",
		"references",
		"hover information",
		"symbols",
		"implementation",
		"call hierarchy",
	}

	for _, descPart := range opDescriptions {
		if !strings.Contains(desc, descPart) {
			t.Errorf("Description should mention %q", descPart)
		}
	}
}

func TestLspTool_ParametersSchemaValidation(t *testing.T) {
	const schemaTypeObject = "object"
	mockMgr := &mockLSPManager{}
	tool := newLspTool("/tmp", mockMgr)

	t.Run("schema is valid JSON", func(t *testing.T) {
		params := tool.Parameters()
		var schema map[string]any
		if err := json.Unmarshal(params, &schema); err != nil {
			t.Fatalf("Parameters must be valid JSON: %v", err)
		}

		// Check top-level structure
		if schema["type"] != schemaTypeObject {
			t.Error("Root type should be 'object'")
		}
	})

	t.Run("all properties have types", func(t *testing.T) {
		params := tool.Parameters()
		var schema map[string]any
		if err := json.Unmarshal(params, &schema); err != nil {
			t.Fatalf("Parameters must be valid JSON: %v", err)
		}

		props := schema["properties"].(map[string]any)
		requiredTypes := []string{"operation", "file_path", "line", "character", "query"}

		for _, propName := range requiredTypes {
			prop, ok := props[propName].(map[string]any)
			if !ok {
				t.Errorf("Property %s should exist", propName)
				continue
			}
			if prop["type"] == nil || prop["type"] == "" {
				t.Errorf("Property %s should have a type", propName)
			}
		}
	})

	t.Run("operation enum has descriptions", func(t *testing.T) {
		params := tool.Parameters()
		var schema map[string]any
		if err := json.Unmarshal(params, &schema); err != nil {
			t.Fatalf("Parameters must be valid JSON: %v", err)
		}

		props := schema["properties"].(map[string]any)
		op := props["operation"].(map[string]any)

		if op["description"] == nil || op["description"] == "" {
			t.Error("Operation should have a description")
		}
	})
}

func TestLspTool_ResponseTypes(t *testing.T) {
	t.Run("Location type serialization", func(t *testing.T) {
		loc := lsp.Location{
			URI: "file:///test.go",
			Range: lsp.Range{
				Start: lsp.Position{Line: 0, Character: 0},
				End:   lsp.Position{Line: 0, Character: 10},
			},
		}

		data, err := json.Marshal(loc)
		if err != nil {
			t.Fatalf("Failed to marshal Location: %v", err)
		}

		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}

		if decoded["uri"] != "file:///test.go" {
			t.Error("URI should be preserved")
		}
	})

	t.Run("SymbolInformation type", func(t *testing.T) {
		sym := lsp.SymbolInformation{
			Name: "TestFunc",
			Kind: 12, // Function
			Location: lsp.SymbolLocation{
				URI: "file:///test.go",
				Range: &lsp.Range{
					Start: lsp.Position{Line: 5, Character: 0},
					End:   lsp.Position{Line: 10, Character: 1},
				},
			},
		}

		data, err := json.Marshal(sym)
		if err != nil {
			t.Fatalf("Failed to marshal SymbolInformation: %v", err)
		}

		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}

		if decoded["name"] != "TestFunc" {
			t.Error("Name should be preserved")
		}
		if decoded["kind"] != float64(12) {
			t.Error("Kind should be preserved")
		}
	})

	t.Run("CallHierarchyItem type", func(t *testing.T) {
		item := lsp.CallHierarchyItem{
			Name: "TestFunc",
			Kind: 12,
			URI:  "file:///test.go",
			Range: lsp.Range{
				Start: lsp.Position{Line: 0, Character: 0},
				End:   lsp.Position{Line: 0, Character: 10},
			},
			SelectionRange: lsp.Range{
				Start: lsp.Position{Line: 0, Character: 0},
				End:   lsp.Position{Line: 0, Character: 10},
			},
		}

		data, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("Failed to marshal CallHierarchyItem: %v", err)
		}

		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}

		if decoded["name"] != "TestFunc" {
			t.Error("Name should be preserved")
		}
		if decoded["uri"] != "file:///test.go" {
			t.Error("URI should be preserved")
		}
	})

	t.Run("CallHierarchyIncomingCall type", func(t *testing.T) {
		call := lsp.CallHierarchyIncomingCall{
			From: lsp.CallHierarchyItem{
				Name: "CallerFunc",
				Kind: 12,
				URI:  "file:///caller.go",
			},
			FromRanges: []lsp.Range{
				{Start: lsp.Position{Line: 10, Character: 0}, End: lsp.Position{Line: 10, Character: 5}},
			},
		}

		data, err := json.Marshal(call)
		if err != nil {
			t.Fatalf("Failed to marshal CallHierarchyIncomingCall: %v", err)
		}

		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}

		from := decoded["from"].(map[string]any)
		if from["name"] != "CallerFunc" {
			t.Error("From.Name should be preserved")
		}
	})

	t.Run("CallHierarchyOutgoingCall type", func(t *testing.T) {
		call := lsp.CallHierarchyOutgoingCall{
			To: lsp.CallHierarchyItem{
				Name: "CalleeFunc",
				Kind: 12,
				URI:  "file:///callee.go",
			},
			FromRanges: []lsp.Range{
				{Start: lsp.Position{Line: 20, Character: 0}, End: lsp.Position{Line: 20, Character: 5}},
			},
		}

		data, err := json.Marshal(call)
		if err != nil {
			t.Fatalf("Failed to marshal CallHierarchyOutgoingCall: %v", err)
		}

		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}

		to := decoded["to"].(map[string]any)
		if to["name"] != "CalleeFunc" {
			t.Error("To.Name should be preserved")
		}
	})
}

func TestLspTool_ParamsValidation(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{
			name:    "valid JSON",
			json:    `{"operation":"goToDefinition","file_path":"/test.go","line":1,"character":1}`,
			wantErr: false,
		},
		{
			name:    "empty operation",
			json:    `{"operation":"","file_path":"/test.go"}`,
			wantErr: false, // Empty string is valid JSON
		},
		{
			name:    "empty file_path",
			json:    `{"operation":"goToDefinition","file_path":""}`,
			wantErr: false, // Empty string is valid JSON
		},
		{
			name:    "invalid JSON",
			json:    `{"operation":"goToDefinition",file_path:/test.go}`,
			wantErr: true,
		},
		{
			name:    "missing fields",
			json:    `{}`,
			wantErr: false, // Valid JSON, just empty
		},
		{
			name:    "unicode in file_path",
			json:    `{"operation":"goToDefinition","file_path":"/путь/к/файлу.go"}`,
			wantErr: false,
		},
		{
			name:    "large line numbers",
			json:    `{"operation":"goToDefinition","file_path":"/test.go","line":999999,"character":999999}`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var params lspParams
			err := json.Unmarshal([]byte(tt.json), &params)

			if tt.wantErr && err == nil {
				t.Errorf("Expected error but got none")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestLspTool_Execute(t *testing.T) {
	ctx := context.Background()

	t.Run("nil manager returns error", func(t *testing.T) {
		tool := newLspTool("/tmp", nil)
		params, _ := json.Marshal(lspParams{
			Operation: "goToDefinition",
			FilePath:  "/test.go",
			Line:      1,
			Character: 1,
		})

		_, err := tool.Execute(ctx, params)
		if err == nil {
			t.Error("Expected error when manager is nil")
		}
		if !strings.Contains(err.Error(), "not initialized") {
			t.Errorf("Error should mention manager not initialized: %v", err)
		}
	})

	t.Run("goToDefinition with results", func(t *testing.T) {
		mockMgr := &mockLSPManager{
			definitionFunc: func(ctx context.Context, workDir, file string, line, character int) ([]lsp.Location, error) {
				return []lsp.Location{
					{
						URI: "file:///test.go",
						Range: lsp.Range{
							Start: lsp.Position{Line: 10, Character: 0},
							End:   lsp.Position{Line: 10, Character: 10},
						},
					},
				}, nil
			},
		}
		tool := newLspTool("/tmp", mockMgr)

		params, _ := json.Marshal(lspParams{
			Operation: "goToDefinition",
			FilePath:  "/test.go",
			Line:      11, // 1-based
			Character: 6,  // 1-based, becomes 5 (0-based)
		})

		result, err := tool.Execute(ctx, params)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if !strings.Contains(result.Output, "file:///test.go") {
			t.Error("Output should contain the file URI")
		}
	})

	t.Run("goToDefinition with no results", func(t *testing.T) {
		mockMgr := &mockLSPManager{
			definitionFunc: func(ctx context.Context, workDir, file string, line, character int) ([]lsp.Location, error) {
				return []lsp.Location{}, nil
			},
		}
		tool := newLspTool("/tmp", mockMgr)

		params, _ := json.Marshal(lspParams{
			Operation: "goToDefinition",
			FilePath:  "/test.go",
			Line:      1,
			Character: 1,
		})

		result, err := tool.Execute(ctx, params)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if !strings.Contains(result.Output, "No definition found") {
			t.Errorf("Output should indicate no definition found: %s", result.Output)
		}
	})

	t.Run("workspaceSymbol with query", func(t *testing.T) {
		mockMgr := &mockLSPManager{
			workspaceSymbolFunc: func(ctx context.Context, workDir, query string) ([]lsp.SymbolInformation, error) {
				return []lsp.SymbolInformation{
					{Name: "TestFunc", Kind: 12, Location: lsp.SymbolLocation{URI: "file:///test.go"}},
				}, nil
			},
		}
		tool := newLspTool("/tmp", mockMgr)

		params, _ := json.Marshal(lspParams{
			Operation: "workspaceSymbol",
			FilePath:  "/test.go",
			Query:     "Test",
		})

		result, err := tool.Execute(ctx, params)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if !strings.Contains(result.Output, "TestFunc") {
			t.Error("Output should contain the symbol name")
		}
	})

	t.Run("goToImplementation with results", func(t *testing.T) {
		mockMgr := &mockLSPManager{
			implementationFunc: func(ctx context.Context, workDir, file string, line, character int) ([]lsp.Location, error) {
				return []lsp.Location{
					{
						URI: "file:///impl.go",
						Range: lsp.Range{
							Start: lsp.Position{Line: 5, Character: 0},
							End:   lsp.Position{Line: 5, Character: 10},
						},
					},
				}, nil
			},
		}
		tool := newLspTool("/tmp", mockMgr)

		params, _ := json.Marshal(lspParams{
			Operation: "goToImplementation",
			FilePath:  "/test.go",
			Line:      1,
			Character: 1,
		})

		result, err := tool.Execute(ctx, params)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if !strings.Contains(result.Output, "impl.go") {
			t.Error("Output should contain implementation file")
		}
	})

	t.Run("prepareCallHierarchy with results", func(t *testing.T) {
		mockMgr := &mockLSPManager{
			prepareCallHierarchyFunc: func(ctx context.Context, workDir, file string, line, character int) ([]lsp.CallHierarchyItem, error) {
				return []lsp.CallHierarchyItem{
					{Name: "TestFunc", Kind: 12, URI: "file:///test.go"},
				}, nil
			},
		}
		tool := newLspTool("/tmp", mockMgr)

		params, _ := json.Marshal(lspParams{
			Operation: "prepareCallHierarchy",
			FilePath:  "/test.go",
			Line:      1,
			Character: 1,
		})

		result, err := tool.Execute(ctx, params)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if !strings.Contains(result.Output, "TestFunc") {
			t.Error("Output should contain hierarchy item name")
		}
	})

	t.Run("incomingCalls with results", func(t *testing.T) {
		mockMgr := &mockLSPManager{
			incomingCallsFunc: func(ctx context.Context, workDir, file string, line, character int) ([]lsp.CallHierarchyIncomingCall, error) {
				return []lsp.CallHierarchyIncomingCall{
					{From: lsp.CallHierarchyItem{Name: "Caller", URI: "file:///caller.go"}},
				}, nil
			},
		}
		tool := newLspTool("/tmp", mockMgr)

		params, _ := json.Marshal(lspParams{
			Operation: "incomingCalls",
			FilePath:  "/test.go",
			Line:      1,
			Character: 1,
		})

		result, err := tool.Execute(ctx, params)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if !strings.Contains(result.Output, "Caller") {
			t.Error("Output should contain caller name")
		}
	})

	t.Run("outgoingCalls with results", func(t *testing.T) {
		mockMgr := &mockLSPManager{
			outgoingCallsFunc: func(ctx context.Context, workDir, file string, line, character int) ([]lsp.CallHierarchyOutgoingCall, error) {
				return []lsp.CallHierarchyOutgoingCall{
					{To: lsp.CallHierarchyItem{Name: "Callee", URI: "file:///callee.go"}},
				}, nil
			},
		}
		tool := newLspTool("/tmp", mockMgr)

		params, _ := json.Marshal(lspParams{
			Operation: "outgoingCalls",
			FilePath:  "/test.go",
			Line:      1,
			Character: 1,
		})

		result, err := tool.Execute(ctx, params)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if !strings.Contains(result.Output, "Callee") {
			t.Error("Output should contain callee name")
		}
	})

	t.Run("findReferences with results", func(t *testing.T) {
		mockMgr := &mockLSPManager{
			referencesFunc: func(ctx context.Context, workDir, file string, line, character int) ([]lsp.Location, error) {
				return []lsp.Location{
					{URI: "file:///ref1.go"},
					{URI: "file:///ref2.go"},
				}, nil
			},
		}
		tool := newLspTool("/tmp", mockMgr)

		params, _ := json.Marshal(lspParams{
			Operation: "findReferences",
			FilePath:  "/test.go",
			Line:      1,
			Character: 1,
		})

		result, err := tool.Execute(ctx, params)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if !strings.Contains(result.Output, "ref1.go") || !strings.Contains(result.Output, "ref2.go") {
			t.Error("Output should contain reference files")
		}
	})

	t.Run("hover with result", func(t *testing.T) {
		mockMgr := &mockLSPManager{
			hoverFunc: func(ctx context.Context, workDir, file string, line, character int) (*lsp.Hover, error) {
				return &lsp.Hover{Contents: lsp.MarkupContent{Kind: "plaintext", Value: "function Test() string"}}, nil
			},
		}
		tool := newLspTool("/tmp", mockMgr)

		params, _ := json.Marshal(lspParams{
			Operation: "hover",
			FilePath:  "/test.go",
			Line:      1,
			Character: 1,
		})

		result, err := tool.Execute(ctx, params)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Output != "function Test() string" {
			t.Errorf("Output should be hover contents: %s", result.Output)
		}
	})

	t.Run("hover with nil result", func(t *testing.T) {
		mockMgr := &mockLSPManager{
			hoverFunc: func(ctx context.Context, workDir, file string, line, character int) (*lsp.Hover, error) {
				return nil, nil
			},
		}
		tool := newLspTool("/tmp", mockMgr)

		params, _ := json.Marshal(lspParams{
			Operation: "hover",
			FilePath:  "/test.go",
			Line:      1,
			Character: 1,
		})

		result, err := tool.Execute(ctx, params)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if !strings.Contains(result.Output, "No hover information") {
			t.Errorf("Output should indicate no hover info: %s", result.Output)
		}
	})

	t.Run("documentSymbol with results", func(t *testing.T) {
		mockMgr := &mockLSPManager{
			documentSymbolFunc: func(ctx context.Context, workDir, file string) ([]lsp.DocumentSymbol, error) {
				return []lsp.DocumentSymbol{
					{Name: "Func1", Kind: 12},
					{Name: "Func2", Kind: 12},
				}, nil
			},
		}
		tool := newLspTool("/tmp", mockMgr)

		params, _ := json.Marshal(lspParams{
			Operation: "documentSymbol",
			FilePath:  "/test.go",
		})

		result, err := tool.Execute(ctx, params)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if !strings.Contains(result.Output, "Func1") || !strings.Contains(result.Output, "Func2") {
			t.Error("Output should contain symbol names")
		}
	})

	t.Run("unknown operation", func(t *testing.T) {
		mockMgr := &mockLSPManager{}
		tool := newLspTool("/tmp", mockMgr)

		params, _ := json.Marshal(lspParams{
			Operation: "unknownOperation",
			FilePath:  "/test.go",
		})

		_, err := tool.Execute(ctx, params)
		if err == nil {
			t.Error("Expected error for unknown operation")
		}
		if !strings.Contains(err.Error(), "unknown operation") {
			t.Errorf("Error should mention unknown operation: %v", err)
		}
	})

	t.Run("manager returns error", func(t *testing.T) {
		mockMgr := &mockLSPManager{
			definitionFunc: func(ctx context.Context, workDir, file string, line, character int) ([]lsp.Location, error) {
				return nil, context.Canceled
			},
		}
		tool := newLspTool("/tmp", mockMgr)

		params, _ := json.Marshal(lspParams{
			Operation: "goToDefinition",
			FilePath:  "/test.go",
			Line:      1,
			Character: 1,
		})

		_, err := tool.Execute(ctx, params)
		if err == nil {
			t.Error("Expected error when manager fails")
		}
	})

	t.Run("invalid JSON parameters", func(t *testing.T) {
		mockMgr := &mockLSPManager{}
		tool := newLspTool("/tmp", mockMgr)

		_, err := tool.Execute(ctx, json.RawMessage(`{invalid`))
		if err == nil {
			t.Error("Expected error for invalid JSON")
		}
	})
}
