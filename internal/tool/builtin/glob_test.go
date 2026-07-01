package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGlobTool_Execute(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "glob_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	if err := os.MkdirAll(filepath.Join(tmpDir, "src", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("main"), 0o644)
	time.Sleep(10 * time.Millisecond) // Ensure different mtimes
	_ = os.WriteFile(filepath.Join(tmpDir, "src", "app.go"), []byte("app"), 0o644)
	time.Sleep(10 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(tmpDir, "src", "pkg", "util.go"), []byte("util"), 0o644)
	_ = os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("readme"), 0o644)

	tool := newGlobTool(tmpDir)

	t.Run("find all go files", func(t *testing.T) {
		params, _ := json.Marshal(globParams{Pattern: "**/*.go"})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		// Should find all .go files
		if !strings.Contains(result.Output, "main.go") {
			t.Error("Should find main.go")
		}
		if !strings.Contains(result.Output, "app.go") {
			t.Error("Should find app.go")
		}
		if !strings.Contains(result.Output, "util.go") {
			t.Error("Should find util.go")
		}

		if strings.Contains(result.Output, "README.md") {
			t.Error("Should not include README.md")
		}

		// Count should be 3
		if result.Metadata["count"] != 3 {
			t.Errorf("Count should be 3, got %v", result.Metadata["count"])
		}
	})

	t.Run("find files in subdirectory", func(t *testing.T) {
		params, _ := json.Marshal(globParams{
			Pattern: "*.go",
			Path:    "src",
		})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "app.go") {
			t.Error("Should find app.go")
		}
		if strings.Contains(result.Output, "main.go") {
			t.Error("Should not find main.go (not in src)")
		}
	})

	t.Run("pattern with specific extension", func(t *testing.T) {
		params, _ := json.Marshal(globParams{Pattern: "*.md"})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "README.md") {
			t.Error("Should find README.md")
		}
		if result.Metadata["count"] != 1 {
			t.Errorf("Count should be 1, got %v", result.Metadata["count"])
		}
	})

	t.Run("no matches", func(t *testing.T) {
		params, _ := json.Marshal(globParams{Pattern: "*.xyz"})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "No files found") {
			t.Error("Should indicate no files found")
		}
		if result.Metadata["count"] != 0 {
			t.Errorf("Count should be 0, got %v", result.Metadata["count"])
		}
	})

	t.Run("empty pattern", func(t *testing.T) {
		params, _ := json.Marshal(globParams{Pattern: ""})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Fatal("Expected error for empty pattern")
		}
	})

	t.Run("nonexistent path", func(t *testing.T) {
		params, _ := json.Marshal(globParams{
			Pattern: "*.go",
			Path:    "nonexistent",
		})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Fatal("Expected error for nonexistent path")
		}
	})

	t.Run("sorted by mtime", func(t *testing.T) {
		params, _ := json.Marshal(globParams{Pattern: "**/*.go"})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		// util.go should be first (newest)
		lines := strings.Split(result.Output, "\n")
		if len(lines) > 0 && !strings.Contains(lines[0], "util.go") {
			t.Errorf("First result should be util.go (newest), got %s", lines[0])
		}
	})
}

func TestGlobTool_Truncation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "glob_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	for i := range 150 {
		_ = os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("file%03d.txt", i)), []byte("content"), 0o644)
	}

	tool := newGlobTool(tmpDir)
	params, _ := json.Marshal(globParams{Pattern: "*.txt"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result.Metadata["truncated"] != true {
		t.Error("Should be truncated")
	}
	if result.Metadata["count"] != 100 {
		t.Errorf("Count should be 100, got %v", result.Metadata["count"])
	}
	if !strings.Contains(result.Output, "truncated") {
		t.Error("Output should mention truncation")
	}
}

func TestGlobTool_Metadata(t *testing.T) {
	tool := newGlobTool("/tmp")

	t.Run("ID", func(t *testing.T) {
		if tool.ID() != "glob" {
			t.Errorf("ID should be 'glob', got %s", tool.ID())
		}
	})

	t.Run("Description", func(t *testing.T) {
		desc := tool.Description()
		if !strings.Contains(desc, "glob") {
			t.Error("Description should mention glob")
		}
	})

	t.Run("Parameters", func(t *testing.T) {
		params := tool.Parameters()
		var schema map[string]any
		if err := json.Unmarshal(params, &schema); err != nil {
			t.Fatalf("Parameters should be valid JSON: %v", err)
		}
	})
}
