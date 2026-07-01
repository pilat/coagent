package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEditTool_StrReplace(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newEditTool(tmpDir, nil, directFileMutator{})

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("line 1\nline 2\nline 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(editParams{
		FilePath:  testFile,
		OldString: "line 2",
		NewString: "replaced line 2",
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	content, _ := os.ReadFile(testFile)
	if string(content) != "line 1\nreplaced line 2\nline 3\n" {
		t.Errorf("unexpected content: %q", string(content))
	}
}

func TestEditTool_StrReplaceMultiLine(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newEditTool(tmpDir, nil, directFileMutator{})

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("aaa\nbbb\nccc\nddd\neee"), 0o644); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(editParams{
		FilePath:  testFile,
		OldString: "bbb\nccc\nddd",
		NewString: "XXX",
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	content, _ := os.ReadFile(testFile)
	if string(content) != "aaa\nXXX\neee" {
		t.Errorf("unexpected content: %q", string(content))
	}
}

func TestEditTool_StrReplaceInsert(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newEditTool(tmpDir, nil, directFileMutator{})

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("line 1\nline 2\nline 3"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Insert a new line after line 2 by including line 2 in oldString
	params, _ := json.Marshal(editParams{
		FilePath:  testFile,
		OldString: "line 2\n",
		NewString: "line 2\nnew inserted line\n",
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	content, _ := os.ReadFile(testFile)
	if string(content) != "line 1\nline 2\nnew inserted line\nline 3" {
		t.Errorf("unexpected content: %q", string(content))
	}
}

func TestEditTool_StrReplaceDelete(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newEditTool(tmpDir, nil, directFileMutator{})

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("line 1\nline 2\nline 3\nline 4"), 0o644); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(editParams{
		FilePath:  testFile,
		OldString: "line 2\nline 3\n",
		NewString: "",
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	content, _ := os.ReadFile(testFile)
	if string(content) != "line 1\nline 4" {
		t.Errorf("unexpected content: %q", string(content))
	}
}

func TestEditTool_StrReplaceNotUnique(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newEditTool(tmpDir, nil, directFileMutator{})

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("foo bar\nbaz foo\nfoo qux"), 0o644); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(editParams{
		FilePath:  testFile,
		OldString: "foo",
		NewString: "replaced",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for non-unique oldString")
	}
	if !strings.Contains(err.Error(), "found 3 times") {
		t.Errorf("error should mention count: %v", err)
	}
}

func TestEditTool_StrReplaceNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newEditTool(tmpDir, nil, directFileMutator{})

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("line 1\nline 2"), 0o644); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(editParams{
		FilePath:  testFile,
		OldString: "nonexistent",
		NewString: "new",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for missing oldString")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found: %v", err)
	}
}

func TestEditTool_ReplaceAll(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newEditTool(tmpDir, nil, directFileMutator{})

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("foo bar foo baz foo"), 0o644); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(editParams{
		FilePath:   testFile,
		OldString:  "foo",
		NewString:  "qux",
		ReplaceAll: true,
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	content, _ := os.ReadFile(testFile)
	if string(content) != "qux bar qux baz qux" {
		t.Errorf("unexpected content: %q", string(content))
	}
}

func TestEditTool_FileNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newEditTool(tmpDir, nil, directFileMutator{})

	params, _ := json.Marshal(editParams{
		FilePath:  filepath.Join(tmpDir, "nonexistent.txt"),
		OldString: "x",
		NewString: "y",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for file not found")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found: %v", err)
	}
}

func TestEditTool_EmptyParams(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newEditTool(tmpDir, nil, directFileMutator{})

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(editParams{FilePath: testFile})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty params")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error should mention required: %v", err)
	}
}

func TestEditTool_ReplaceAllWithOldNew(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newEditTool(tmpDir, nil, directFileMutator{})

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("a x a x a"), 0o644); err != nil {
		t.Fatal(err)
	}

	// replaceAll works together with oldString/newString (classic schema).
	params, _ := json.Marshal(editParams{
		FilePath:   testFile,
		OldString:  "a",
		NewString:  "b",
		ReplaceAll: true,
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	content, _ := os.ReadFile(testFile)
	if string(content) != "b x b x b" {
		t.Errorf("unexpected content: %q", string(content))
	}
}

func TestEditTool_RelativePath(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newEditTool(tmpDir, nil, directFileMutator{})

	testFile := filepath.Join(tmpDir, "rel.txt")
	if err := os.WriteFile(testFile, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(editParams{
		FilePath:  "rel.txt",
		OldString: "hello",
		NewString: "goodbye",
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	content, _ := os.ReadFile(testFile)
	if string(content) != "goodbye world" {
		t.Errorf("unexpected content: %q", string(content))
	}
}

func TestEditTool_ContextPreview(t *testing.T) {
	makeFile := func(t *testing.T, dir string, n int) string {
		t.Helper()
		var lines []string
		for i := 1; i <= n; i++ {
			lines = append(lines, fmt.Sprintf("line %d", i))
		}
		path := filepath.Join(dir, "test.txt")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("shows_context_around_edit", func(t *testing.T) {
		tmpDir := t.TempDir()
		tool := newEditTool(tmpDir, nil, directFileMutator{})
		makeFile(t, tmpDir, 20)

		params, _ := json.Marshal(editParams{
			FilePath:  "test.txt",
			OldString: "line 10",
			NewString: "replaced 10",
		})

		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "Result around edit") {
			t.Errorf("expected context preview, got:\n%s", result.Output)
		}
		if !strings.Contains(result.Output, "replaced 10") {
			t.Errorf("expected replaced content in preview, got:\n%s", result.Output)
		}
		// Preview should NOT contain hashes
		if strings.Contains(result.Output, ":") && strings.Contains(result.Output, "| line") {
			// Check it's not hashline format "N:XX| content"
			for line := range strings.SplitSeq(result.Output, "\n") {
				if strings.Contains(line, "| ") && len(line) > 3 {
					parts := strings.SplitN(line, "| ", 2)
					if strings.Contains(parts[0], ":") && !strings.Contains(parts[0], "lines") {
						t.Errorf("preview should not use hashline format, got line: %s", line)
					}
				}
			}
		}
	})

	t.Run("replaceAll_tiered", func(t *testing.T) {
		tmpDir := t.TempDir()
		tool := newEditTool(tmpDir, nil, directFileMutator{})
		testFile := filepath.Join(tmpDir, "test.txt")
		var lines []string
		for i := range 30 {
			if i%6 == 0 {
				lines = append(lines, "xx placeholder")
			} else {
				lines = append(lines, fmt.Sprintf("line %d", i))
			}
		}
		if err := os.WriteFile(testFile, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}

		params, _ := json.Marshal(editParams{
			FilePath:   testFile,
			OldString:  "xx",
			NewString:  "yy",
			ReplaceAll: true,
		})

		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "and 2 more replacements") {
			t.Errorf("expected tiered output, got:\n%s", result.Output)
		}
	})
}

func TestEditTool_Metadata(t *testing.T) {
	tool := newEditTool("/tmp", nil, directFileMutator{})

	if tool.ID() != "edit" {
		t.Errorf("ID should be 'edit', got %s", tool.ID())
	}

	desc := tool.Description()
	if !strings.Contains(desc, "string matching") {
		t.Error("Description should mention string matching")
	}

	params := tool.Parameters()
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters should be valid JSON: %v", err)
	}
}

func TestEditTool_DelegatesMutation(t *testing.T) {
	want := errors.New("edit denied")
	mutator := &recordingFileMutator{err: want}
	workDir := t.TempDir()
	tool := newEditTool(workDir, nil, mutator)
	path := filepath.Join(workDir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o644))
	params, err := json.Marshal(editParams{
		FilePath:  path,
		OldString: "before",
		NewString: "after",
	})
	require.NoError(t, err)

	ctx := context.WithValue(context.Background(), mutationContextKey{}, "marker")
	result, err := tool.Execute(ctx, params)

	require.ErrorIs(t, err, want)
	assert.Nil(t, result)
	require.Len(t, mutator.calls, 1)
	assert.Equal(t, path, mutator.calls[0].path)
	assert.Equal(t, []byte("after"), mutator.calls[0].content)
	assert.False(t, mutator.calls[0].createParents)
	assert.Equal(t, "marker", mutator.calls[0].ctx.Value(mutationContextKey{}))

	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "before", string(content))
}
