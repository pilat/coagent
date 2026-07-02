package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTool_Execute(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "read_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	content := "line 1\nline 2\nline 3\nline 4\nline 5\n"
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := newReadTool(tmpDir)

	t.Run("basic read", func(t *testing.T) {
		params, _ := json.Marshal(readParams{FilePath: testFile})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "1| line 1") {
			t.Error("Output should contain line 1")
		}
		if !strings.Contains(result.Output, "5| line 5") {
			t.Error("Output should contain line 5")
		}
		if !strings.Contains(result.Output, "total 5 lines") {
			t.Error("Output should indicate total lines")
		}
	})

	t.Run("read with offset", func(t *testing.T) {
		params, _ := json.Marshal(readParams{FilePath: testFile, Offset: 2})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if strings.Contains(result.Output, "1| line 1") {
			t.Error("Output should not contain line 1")
		}
		if !strings.Contains(result.Output, "3| line 3") {
			t.Error("Output should start with line 3")
		}
	})

	t.Run("read with limit", func(t *testing.T) {
		params, _ := json.Marshal(readParams{FilePath: testFile, Limit: 2})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "2| line 2") {
			t.Error("Output should contain line 2")
		}
		if strings.Contains(result.Output, "3| line 3") {
			t.Error("Output should not contain line 3")
		}
		if !strings.Contains(result.Output, "File has more lines") {
			t.Error("Output should indicate more lines available")
		}
	})

	t.Run("read relative path", func(t *testing.T) {
		params, _ := json.Marshal(readParams{FilePath: "test.txt"})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "line 1") {
			t.Error("Should read file using relative path")
		}
	})

	t.Run("file not found", func(t *testing.T) {
		params, _ := json.Marshal(readParams{FilePath: "nonexistent.txt"})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Fatal("Expected error for nonexistent file")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("Error should mention file not found: %v", err)
		}
	})

	t.Run("directory instead of file", func(t *testing.T) {
		params, _ := json.Marshal(readParams{FilePath: tmpDir})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Fatal("Expected error for directory")
		}
		if !strings.Contains(err.Error(), "directory") {
			t.Errorf("Error should mention directory: %v", err)
		}
	})

	t.Run("empty filePath", func(t *testing.T) {
		params, _ := json.Marshal(readParams{FilePath: ""})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Fatal("Expected error for empty filePath")
		}
	})
}

func TestReadTool_LongLines(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "read_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	longLine := strings.Repeat("x", 3000)
	testFile := filepath.Join(tmpDir, "long.txt")
	if err := os.WriteFile(testFile, []byte(longLine+"\nshort\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := newReadTool(tmpDir)
	params, _ := json.Marshal(readParams{FilePath: testFile})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Line should be truncated
	if !strings.Contains(result.Output, "...") {
		t.Error("Long line should be truncated with ...")
	}
	// Should still have line 2
	if !strings.Contains(result.Output, "2| short") {
		t.Error("Should still contain short second line")
	}
}

func TestReadTool_BinaryFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "read_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	binaryContent := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE}
	testFile := filepath.Join(tmpDir, "binary.bin")
	if err := os.WriteFile(testFile, binaryContent, 0o644); err != nil {
		t.Fatal(err)
	}

	tool := newReadTool(tmpDir)
	params, _ := json.Marshal(readParams{FilePath: testFile})
	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("Expected error for binary file")
	}
	if !strings.Contains(err.Error(), "binary") {
		t.Errorf("Error should mention binary: %v", err)
	}
}

func TestReadTool_Metadata(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "read_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := newReadTool(tmpDir)

	t.Run("ID", func(t *testing.T) {
		if tool.ID() != "read" {
			t.Errorf("ID should be 'read', got %s", tool.ID())
		}
	})

	t.Run("Description", func(t *testing.T) {
		desc := tool.Description()
		if !strings.Contains(desc, "Read") {
			t.Error("Description should mention reading")
		}
	})

	t.Run("Parameters", func(t *testing.T) {
		params := tool.Parameters()
		var schema map[string]any
		if err := json.Unmarshal(params, &schema); err != nil {
			t.Fatalf("Parameters should be valid JSON: %v", err)
		}
		if schema["type"] != "object" {
			t.Error("Parameters should define an object schema")
		}
	})
}
