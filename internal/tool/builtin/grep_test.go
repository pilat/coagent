package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepTool_Execute(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "grep_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	_ = os.WriteFile(filepath.Join(tmpDir, "file1.txt"), []byte("hello world\nfoo bar\nhello again"), 0o644)
	_ = os.WriteFile(
		filepath.Join(tmpDir, "file2.go"),
		[]byte("package main\nfunc main() {\n\tprintln(\"hello\")\n}"),
		0o644,
	)
	if err := os.MkdirAll(filepath.Join(tmpDir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tmpDir, "subdir", "file3.txt"), []byte("hello from subdir"), 0o644)

	tool := newGrepTool(tmpDir)

	t.Run("simple pattern", func(t *testing.T) {
		params, _ := json.Marshal(grepParams{Pattern: "hello"})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "hello world") {
			t.Error("Should find 'hello world'")
		}
		if !strings.Contains(result.Output, "hello again") {
			t.Error("Should find 'hello again'")
		}
		if result.Metadata["matches"].(int) < 3 {
			t.Errorf("Should have at least 3 matches, got %v", result.Metadata["matches"])
		}
	})

	t.Run("with glob filter", func(t *testing.T) {
		params, _ := json.Marshal(grepParams{
			Pattern: "hello",
			Glob:    "*.txt",
		})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if strings.Contains(result.Output, "file2.go") {
			t.Error("Should not match .go files when glob is *.txt")
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		params, _ := json.Marshal(grepParams{
			Pattern:    "HELLO",
			IgnoreCase: true,
		})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "hello") {
			t.Error("Should find 'hello' with case-insensitive search")
		}
	})

	t.Run("files only", func(t *testing.T) {
		params, _ := json.Marshal(grepParams{
			Pattern:   "hello",
			FilesOnly: true,
		})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		// Should only contain file paths, not content
		if strings.Contains(result.Output, "hello world") {
			t.Error("Files-only mode should not show matching content")
		}
		if !strings.Contains(result.Output, "file1.txt") {
			t.Error("Should show file path")
		}
	})

	t.Run("no matches", func(t *testing.T) {
		params, _ := json.Marshal(grepParams{Pattern: "nonexistent"})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "No matches") {
			t.Error("Should indicate no matches")
		}
		if result.Metadata["matches"] != 0 {
			t.Errorf("Matches should be 0, got %v", result.Metadata["matches"])
		}
	})

	t.Run("regex pattern", func(t *testing.T) {
		params, _ := json.Marshal(grepParams{Pattern: `func \w+\(`})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "func main()") {
			t.Error("Should match regex pattern")
		}
	})

	t.Run("invalid regex", func(t *testing.T) {
		params, _ := json.Marshal(grepParams{Pattern: "[invalid"})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Fatal("Expected error for invalid regex")
		}
		if !strings.Contains(err.Error(), "invalid regex") {
			t.Errorf("Error should mention invalid regex: %v", err)
		}
	})

	t.Run("empty pattern", func(t *testing.T) {
		params, _ := json.Marshal(grepParams{Pattern: ""})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Fatal("Expected error for empty pattern")
		}
	})

	t.Run("specific path", func(t *testing.T) {
		params, _ := json.Marshal(grepParams{
			Pattern: "hello",
			Path:    "subdir",
		})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "subdir") {
			t.Error("Should find matches in subdir")
		}
		if strings.Contains(result.Output, "file1.txt") {
			t.Error("Should not include files outside path")
		}
	})
}

func TestGrepTool_WithContext(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "grep_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	content := "line 1\nline 2\nMATCH\nline 4\nline 5"
	_ = os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte(content), 0o644)

	tool := newGrepTool(tmpDir)

	params, _ := json.Marshal(grepParams{
		Pattern: "MATCH",
		Context: 2,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Should include context lines
	if !strings.Contains(result.Output, "line 1") || !strings.Contains(result.Output, "line 2") {
		t.Error("Should include context lines before match")
	}
}

func TestGrepTool_Metadata(t *testing.T) {
	tool := newGrepTool("/tmp")

	if tool.ID() != "grep" {
		t.Errorf("ID should be 'grep', got %s", tool.ID())
	}

	desc := tool.Description()
	if !strings.Contains(desc, "regex") {
		t.Error("Description should mention regex")
	}

	params := tool.Parameters()
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters should be valid JSON: %v", err)
	}
}
