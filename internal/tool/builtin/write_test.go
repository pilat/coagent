package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteTool_Execute(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "write_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tool := newWriteTool(tmpDir, nil, directFileMutator{})

	t.Run("create new file", func(t *testing.T) {
		testFile := filepath.Join(tmpDir, "new.txt")
		content := "hello world"

		params, _ := json.Marshal(writeParams{FilePath: testFile, Content: content})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		data, err := os.ReadFile(testFile)
		if err != nil {
			t.Fatalf("File should exist: %v", err)
		}
		if string(data) != content {
			t.Errorf("File content mismatch: got %q, want %q", string(data), content)
		}

		if !strings.Contains(result.Output, "created") {
			t.Error("Output should mention file was created")
		}
		if result.Metadata["isNew"] != true {
			t.Error("Metadata should indicate new file")
		}
	})

	t.Run("overwrite existing file", func(t *testing.T) {
		testFile := filepath.Join(tmpDir, "existing.txt")
		if err := os.WriteFile(testFile, []byte("old content"), 0o644); err != nil {
			t.Fatal(err)
		}

		newContent := "new content"
		params, _ := json.Marshal(writeParams{FilePath: testFile, Content: newContent})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		data, err := os.ReadFile(testFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != newContent {
			t.Errorf("File should be overwritten: got %q, want %q", string(data), newContent)
		}

		if !strings.Contains(result.Output, "updated") {
			t.Error("Output should mention file was updated")
		}
		if result.Metadata["isNew"] != false {
			t.Error("Metadata should indicate existing file")
		}
	})

	t.Run("create nested directories", func(t *testing.T) {
		testFile := filepath.Join(tmpDir, "a", "b", "c", "nested.txt")
		content := "nested content"

		params, _ := json.Marshal(writeParams{FilePath: testFile, Content: content})
		_, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		data, err := os.ReadFile(testFile)
		if err != nil {
			t.Fatalf("File should exist: %v", err)
		}
		if string(data) != content {
			t.Errorf("Content mismatch: got %q, want %q", string(data), content)
		}
	})

	t.Run("relative path", func(t *testing.T) {
		content := "relative content"

		params, _ := json.Marshal(writeParams{FilePath: "relative.txt", Content: content})
		_, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(tmpDir, "relative.txt"))
		if err != nil {
			t.Fatalf("File should exist at workDir: %v", err)
		}
		if string(data) != content {
			t.Errorf("Content mismatch: got %q, want %q", string(data), content)
		}
	})

	t.Run("empty filePath", func(t *testing.T) {
		params, _ := json.Marshal(writeParams{FilePath: "", Content: "content"})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Fatal("Expected error for empty filePath")
		}
	})

	t.Run("directory instead of file", func(t *testing.T) {
		dirPath := filepath.Join(tmpDir, "subdir")
		if err := os.Mkdir(dirPath, 0o755); err != nil {
			t.Fatal(err)
		}

		params, _ := json.Marshal(writeParams{FilePath: dirPath, Content: "content"})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Fatal("Expected error when writing to directory")
		}
		if !strings.Contains(err.Error(), "directory") {
			t.Errorf("Error should mention directory: %v", err)
		}
	})
}

func TestWriteTool_Metadata(t *testing.T) {
	tool := newWriteTool("/tmp", nil, directFileMutator{})

	t.Run("ID", func(t *testing.T) {
		if tool.ID() != "write" {
			t.Errorf("ID should be 'write', got %s", tool.ID())
		}
	})

	t.Run("Description", func(t *testing.T) {
		desc := tool.Description()
		if !strings.Contains(desc, "Write") {
			t.Error("Description should mention writing")
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

func TestWriteTool_EmptyContent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "write_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tool := newWriteTool(tmpDir, nil, directFileMutator{})
	testFile := filepath.Join(tmpDir, "empty.txt")

	params, _ := json.Marshal(writeParams{FilePath: testFile, Content: ""})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Empty file should be created
	data, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("File should exist: %v", err)
	}
	if len(data) != 0 {
		t.Error("File should be empty")
	}

	if result.Metadata["bytes"] != 0 {
		t.Errorf("Bytes should be 0, got %v", result.Metadata["bytes"])
	}
}

func TestWriteTool_DelegatesMutation(t *testing.T) {
	want := errors.New("write denied")
	mutator := &recordingFileMutator{err: want}
	workDir := t.TempDir()
	tool := newWriteTool(workDir, nil, mutator)
	path := filepath.Join(workDir, "nested", "file.txt")
	params, err := json.Marshal(writeParams{FilePath: path, Content: "content"})
	require.NoError(t, err)

	ctx := context.WithValue(context.Background(), mutationContextKey{}, "marker")
	result, err := tool.Execute(ctx, params)

	require.ErrorIs(t, err, want)
	assert.Nil(t, result)
	require.Len(t, mutator.calls, 1)
	assert.Equal(t, path, mutator.calls[0].path)
	assert.Equal(t, []byte("content"), mutator.calls[0].content)
	assert.True(t, mutator.calls[0].createParents)
	assert.Equal(t, "marker", mutator.calls[0].ctx.Value(mutationContextKey{}))
	assert.NoFileExists(t, path)
	assert.NoDirExists(t, filepath.Dir(path))
}
