//go:build integration

package builtin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilat/coagent/internal/lsp"
)

// Run with: go test -tags=integration ./internal/tool/builtin/...
func TestIntegration_LspToolExecute(t *testing.T) {
	_, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not found in PATH, skipping integration tests")
	}

	tmpDir, err := os.MkdirTemp("", "lsp_tool_integration_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	goModContent := `module testmodule

go 1.21
`
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goModContent), 0o644); err != nil {
		t.Fatalf("Failed to write go.mod: %v", err)
	}

	testFile := filepath.Join(tmpDir, "test.go")
	goContent := `package testmodule

// Calculator is an interface
type Calculator interface {
	Add(a, b int) int
}

// BasicCalculator implements Calculator
type BasicCalculator struct{}

func (b *BasicCalculator) Add(a, b int) int {
	return a + b
}

func UseCalculator(c Calculator) int {
	return c.Add(1, 2)
}
`
	if err := os.WriteFile(testFile, []byte(goContent), 0o644); err != nil {
		t.Fatalf("Failed to write test.go: %v", err)
	}

	mgr := lsp.NewManager(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Diagnostics resolves the client for this workdir/extension and opens the document.
	if _, err := mgr.GetDiagnostics(ctx, tmpDir, testFile); err != nil {
		t.Logf("GetDiagnostics warning: %v", err)
	}

	tool := newLspTool(tmpDir, mgr)

	t.Run("goToDefinition", func(t *testing.T) {
		params := lspParams{
			Operation: "goToDefinition",
			FilePath:  testFile,
			Line:      6, // Calculator interface
			Character: 5,
		}

		data, _ := json.Marshal(params)
		result, err := tool.Execute(ctx, data)
		if err != nil {
			t.Logf("goToDefinition error: %v", err)
			return
		}
		t.Logf("goToDefinition result: %s", result.Output)
	})

	t.Run("hover", func(t *testing.T) {
		params := lspParams{
			Operation: "hover",
			FilePath:  testFile,
			Line:      11, // BasicCalculator struct
			Character: 5,
		}

		data, _ := json.Marshal(params)
		result, err := tool.Execute(ctx, data)
		if err != nil {
			t.Logf("hover error: %v", err)
			return
		}
		t.Logf("hover result: %s", result.Output)
	})

	t.Run("documentSymbol", func(t *testing.T) {
		params := lspParams{
			Operation: "documentSymbol",
			FilePath:  testFile,
			Line:      1,
			Character: 1,
		}

		data, _ := json.Marshal(params)
		result, err := tool.Execute(ctx, data)
		if err != nil {
			t.Logf("documentSymbol error: %v", err)
			return
		}
		t.Logf("documentSymbol result length: %d", len(result.Output))
		if len(result.Output) > 100 {
			t.Logf("documentSymbol result preview: %.100s...", result.Output)
		}
	})

	t.Run("goToImplementation", func(t *testing.T) {
		params := lspParams{
			Operation: "goToImplementation",
			FilePath:  testFile,
			Line:      6, // Calculator interface
			Character: 5,
		}

		data, _ := json.Marshal(params)
		result, err := tool.Execute(ctx, data)
		if err != nil {
			t.Logf("goToImplementation error: %v", err)
			return
		}
		t.Logf("goToImplementation result: %s", result.Output)
	})

	t.Run("findReferences", func(t *testing.T) {
		params := lspParams{
			Operation: "findReferences",
			FilePath:  testFile,
			Line:      6, // Calculator interface
			Character: 5,
		}

		data, _ := json.Marshal(params)
		result, err := tool.Execute(ctx, data)
		if err != nil {
			t.Logf("findReferences error: %v", err)
			return
		}
		t.Logf("findReferences result: %s", result.Output)
	})

	t.Run("prepareCallHierarchy", func(t *testing.T) {
		params := lspParams{
			Operation: "prepareCallHierarchy",
			FilePath:  testFile,
			Line:      17, // UseCalculator function
			Character: 5,
		}

		data, _ := json.Marshal(params)
		result, err := tool.Execute(ctx, data)
		if err != nil {
			t.Logf("prepareCallHierarchy error: %v", err)
			return
		}
		t.Logf("prepareCallHierarchy result: %s", result.Output)
	})

	t.Run("workspaceSymbol", func(t *testing.T) {
		params := lspParams{
			Operation: "workspaceSymbol",
			FilePath:  testFile,
			Query:     "Calculator",
		}

		data, _ := json.Marshal(params)
		result, err := tool.Execute(ctx, data)
		if err != nil {
			t.Logf("workspaceSymbol error: %v", err)
			return
		}
		t.Logf("workspaceSymbol result: %s", result.Output)
	})

	t.Run("incomingCalls", func(t *testing.T) {
		params := lspParams{
			Operation: "incomingCalls",
			FilePath:  testFile,
			Line:      12, // Add method
			Character: 5,
		}

		data, _ := json.Marshal(params)
		result, err := tool.Execute(ctx, data)
		if err != nil {
			t.Logf("incomingCalls error: %v", err)
			return
		}
		t.Logf("incomingCalls result: %s", result.Output)
	})

	t.Run("outgoingCalls", func(t *testing.T) {
		params := lspParams{
			Operation: "outgoingCalls",
			FilePath:  testFile,
			Line:      17, // UseCalculator function
			Character: 5,
		}

		data, _ := json.Marshal(params)
		result, err := tool.Execute(ctx, data)
		if err != nil {
			t.Logf("outgoingCalls error: %v", err)
			return
		}
		t.Logf("outgoingCalls result: %s", result.Output)
	})
}
