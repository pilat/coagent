package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLsTool_Execute(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ls_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	_ = os.WriteFile(filepath.Join(tmpDir, "file1.txt"), []byte("content1"), 0o644)
	_ = os.WriteFile(filepath.Join(tmpDir, "file2.go"), []byte("content22"), 0o644)
	if err := os.Mkdir(filepath.Join(tmpDir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tmpDir, ".hidden"), []byte("hidden"), 0o644)

	tool := NewLsTool(tmpDir)

	t.Run("list directory", func(t *testing.T) {
		params, _ := json.Marshal(LsParams{Path: tmpDir})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		// Check directories come first
		subdirIdx := strings.Index(result.Output, "subdir/")
		file1Idx := strings.Index(result.Output, "file1.txt")
		if subdirIdx == -1 || file1Idx == -1 {
			t.Errorf("Output should contain subdir and file1.txt: %s", result.Output)
		}
		if subdirIdx > file1Idx {
			t.Error("Directories should come before files")
		}

		// Check hidden file is excluded
		if strings.Contains(result.Output, ".hidden") {
			t.Error("Hidden files should be excluded")
		}

		// Check file sizes are shown
		if !strings.Contains(result.Output, "file1.txt (") {
			t.Error("Files should show sizes")
		}
	})

	t.Run("relative path", func(t *testing.T) {
		params, _ := json.Marshal(LsParams{Path: "."})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "file1.txt") {
			t.Error("Should list files in working directory")
		}
	})

	t.Run("empty directory", func(t *testing.T) {
		emptyDir := filepath.Join(tmpDir, "empty")
		if err := os.Mkdir(emptyDir, 0o755); err != nil {
			t.Fatal(err)
		}

		params, _ := json.Marshal(LsParams{Path: emptyDir})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "empty directory") {
			t.Error("Should indicate empty directory")
		}
	})

	t.Run("path not found", func(t *testing.T) {
		params, _ := json.Marshal(LsParams{Path: "nonexistent"})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Fatal("Expected error for nonexistent path")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("Error should mention not found: %v", err)
		}
	})

	t.Run("path is file not directory", func(t *testing.T) {
		params, _ := json.Marshal(LsParams{Path: filepath.Join(tmpDir, "file1.txt")})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Fatal("Expected error for file path")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("Error should mention not a directory: %v", err)
		}
	})
}

func TestLsTool_Metadata(t *testing.T) {
	tool := NewLsTool("/tmp")

	t.Run("ID", func(t *testing.T) {
		if tool.ID() != "ls" {
			t.Errorf("ID should be 'ls', got %s", tool.ID())
		}
	})

	t.Run("Description", func(t *testing.T) {
		desc := tool.Description()
		if !strings.Contains(desc, "List") {
			t.Error("Description should mention listing")
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
