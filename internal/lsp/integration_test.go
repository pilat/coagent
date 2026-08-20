//go:build integration

package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Run with: go test -tags=integration ./internal/lsp/...
func TestIntegration_LSPOperations(t *testing.T) {
	_, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not found in PATH, skipping integration tests")
	}

	tmpDir, err := os.MkdirTemp("", "lsp_integration_test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	goModContent := `module testmodule

go 1.21
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goModContent), 0o644))

	testFile := filepath.Join(tmpDir, "test.go")
	goContent := `package testmodule

// Greeter is an interface
type Greeter interface {
	Greet(name string) string
}

// EnglishGreeter implements Greeter
type EnglishGreeter struct{}

func (e *EnglishGreeter) Greet(name string) string {
	return "Hello, " + name
}

// SpanishGreeter implements Greeter
type SpanishGreeter struct{}

func (s *SpanishGreeter) Greet(name string) string {
	return "Hola, " + name
}

// UseGreeter calls the Greeter interface
func UseGreeter(g Greeter, name string) string {
	return g.Greet(name)
}

// MainFunc is the entry point
func MainFunc() {
	english := &EnglishGreeter{}
	_ = UseGreeter(english, "World")
}
`
	require.NoError(t, os.WriteFile(testFile, []byte(goContent), 0o644))

	mgr := NewManager(nil)
	t.Cleanup(func() { mgr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("Definition", func(t *testing.T) {
		locs, err := mgr.Definition(ctx, tmpDir, testFile, 4, 5)
		require.NoError(t, err)
		assert.NotEmpty(t, locs, "should find at least one definition location")
	})

	t.Run("Hover", func(t *testing.T) {
		hover, err := mgr.Hover(ctx, tmpDir, testFile, 8, 5)
		require.NoError(t, err)
		assert.NotNil(t, hover)
		assert.NotEmpty(t, hover.Contents.Value, "hover should have content")
	})

	t.Run("DocumentSymbol", func(t *testing.T) {
		symbols, err := mgr.DocumentSymbol(ctx, tmpDir, testFile)
		require.NoError(t, err)
		assert.NotEmpty(t, symbols, "should find symbols in the file")

		names := make([]string, 0, len(symbols))
		for _, sym := range symbols {
			names = append(names, sym.Name)
		}
		assert.Contains(t, names, "Greeter")
	})

	t.Run("Implementation", func(t *testing.T) {
		locs, err := mgr.Implementation(ctx, tmpDir, testFile, 4, 5)
		require.NoError(t, err)
		assert.NotEmpty(t, locs, "Greeter interface should have implementations")
	})

	t.Run("References", func(t *testing.T) {
		locs, err := mgr.References(ctx, tmpDir, testFile, 4, 5)
		require.NoError(t, err)
		assert.NotEmpty(t, locs, "Greeter should have references")
	})

	t.Run("PrepareCallHierarchy", func(t *testing.T) {
		items, err := mgr.PrepareCallHierarchy(ctx, tmpDir, testFile, 27, 5)
		require.NoError(t, err)
		assert.NotEmpty(t, items, "MainFunc should have call hierarchy items")
	})

	t.Run("IncomingCalls", func(t *testing.T) {
		// UseGreeter at line 23 should have incoming call from MainFunc.
		calls, err := mgr.IncomingCalls(ctx, tmpDir, testFile, 22, 5)
		require.NoError(t, err)
		assert.NotEmpty(t, calls, "UseGreeter should have incoming calls")
	})

	t.Run("OutgoingCalls", func(t *testing.T) {
		// MainFunc at line 28 calls UseGreeter.
		calls, err := mgr.OutgoingCalls(ctx, tmpDir, testFile, 27, 5)
		require.NoError(t, err)
		assert.NotEmpty(t, calls, "MainFunc should have outgoing calls")
	})

	t.Run("WorkspaceSymbol", func(t *testing.T) {
		symbols, err := mgr.WorkspaceSymbol(ctx, tmpDir, testFile, "Greeter")
		require.NoError(t, err)
		assert.NotEmpty(t, symbols, "should find Greeter in workspace symbols")
	})
}
