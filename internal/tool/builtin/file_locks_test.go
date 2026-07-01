package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// delayedMutator widens the window between a tool's read and its write.
type delayedMutator struct {
	delay time.Duration
}

// streamingMutator reproduces the sandbox mutator's `cat > file`: truncate at
// open, then stream the content in chunks at the writer's own offset. Two
// concurrent streams leave the longer content's tail past the shorter one.
type streamingMutator struct {
	chunks int
}

func (m delayedMutator) WriteFile(ctx context.Context, path string, content []byte, createParents bool) error {
	time.Sleep(m.delay)

	return directFileMutator{}.WriteFile(ctx, path, content, createParents)
}

func (m streamingMutator) WriteFile(_ context.Context, path string, content []byte, _ bool) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}

	defer func() { _ = file.Close() }()

	size := max(1, (len(content)+m.chunks-1)/m.chunks)

	for start := 0; start < len(content); start += size {
		if _, err := file.Write(content[start:min(start+size, len(content))]); err != nil {
			return fmt.Errorf("write: %w", err)
		}

		time.Sleep(time.Millisecond)
	}

	return nil
}

// TestConcurrentEditsKeepEveryEdit covers the lost update: tool calls of one
// assistant response run concurrently, and every edit rewrites the whole file
// from its own read.
func TestConcurrentEditsKeepEveryEdit(t *testing.T) {
	t.Parallel()

	const lines = 8

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "target.txt")

	var before, after strings.Builder
	for i := range lines {
		fmt.Fprintf(&before, "line %d\n", i)
		fmt.Fprintf(&after, "edited %d\n", i)
	}

	require.NoError(t, os.WriteFile(testFile, []byte(before.String()), 0o644))

	editTool := newEditTool(tmpDir, nil, delayedMutator{delay: 2 * time.Millisecond})

	var wg sync.WaitGroup

	errs := make([]error, lines)

	for i := range lines {
		wg.Go(func() {
			params, _ := json.Marshal(editParams{
				FilePath:  testFile,
				OldString: fmt.Sprintf("line %d\n", i),
				NewString: fmt.Sprintf("edited %d\n", i),
			})

			_, errs[i] = editTool.Execute(context.Background(), params)
		})
	}

	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "edit %d", i)
	}

	content, err := os.ReadFile(testFile)
	require.NoError(t, err)
	assert.Equal(t, after.String(), string(content))
}

// TestConcurrentEditsLeaveNoTornTail is the session-114 regression: two
// concurrent edits of unequal length left the longer content's last bytes
// dangling past the end of the shorter one — a stray `)` and `}` after the
// final function, which broke the build.
func TestConcurrentEditsLeaveNoTornTail(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "target.go")

	base := strings.Repeat("// filler line to make the file worth streaming\n", 200) +
		"// alpha\n" +
		strings.Repeat("// more filler\n", 200) +
		"// beta\n" +
		"func tail() {\n\treturn\n}\n"

	longEdit := "// alpha rewritten with a comment long enough to change the file length\n"
	shortEdit := "// beta rewritten\n"

	require.NoError(t, os.WriteFile(testFile, []byte(base), 0o644))

	editTool := newEditTool(tmpDir, nil, streamingMutator{chunks: 4})

	var wg sync.WaitGroup

	edits := [][2]string{{"// alpha\n", longEdit}, {"// beta\n", shortEdit}}
	errs := make([]error, len(edits))

	for i, edit := range edits {
		wg.Go(func() {
			params, _ := json.Marshal(editParams{FilePath: testFile, OldString: edit[0], NewString: edit[1]})
			_, errs[i] = editTool.Execute(context.Background(), params)
		})
	}

	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "edit %d", i)
	}

	content, err := os.ReadFile(testFile)
	require.NoError(t, err)

	want := strings.Replace(base, "// alpha\n", longEdit, 1)
	want = strings.Replace(want, "// beta\n", shortEdit, 1)

	assert.Equal(t, want, string(content))
	assert.True(t, strings.HasSuffix(string(content), "func tail() {\n\treturn\n}\n"),
		"file tail carries residue from the other writer")
}

// TestReadNeverObservesPartialWrite covers a read issued in the same batch as a
// mutation: the streaming write truncates first, so an unsynchronized scan can
// return a file missing everything after the current chunk.
func TestReadNeverObservesPartialWrite(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "target.txt")

	base := strings.Repeat("payload line\n", 100) + "MARKER\nEND\n"
	require.NoError(t, os.WriteFile(testFile, []byte(base), 0o644))

	editTool := newEditTool(tmpDir, nil, streamingMutator{chunks: 8})
	readTool := newReadTool(tmpDir)

	var wg sync.WaitGroup

	wg.Go(func() {
		params, _ := json.Marshal(editParams{
			FilePath:  testFile,
			OldString: "MARKER\n",
			NewString: "MARKER rewritten to a longer line\n",
		})

		_, err := editTool.Execute(context.Background(), params)
		assert.NoError(t, err)
	})

	outputs := make([]string, 20)

	for i := range outputs {
		wg.Go(func() {
			params, _ := json.Marshal(readParams{FilePath: testFile})

			result, err := readTool.Execute(context.Background(), params)
			if assert.NoError(t, err) {
				outputs[i] = result.Output
			}
		})
	}

	wg.Wait()

	for i, out := range outputs {
		assert.Containsf(t, out, "END", "read %d saw a truncated file", i)
	}
}

func TestPathLocksReleaseEntries(t *testing.T) {
	t.Parallel()

	locks := &pathLocks{entries: make(map[string]*pathLockEntry)}

	first, key := locks.acquire("/tmp/a/../a/file.txt")
	second, sameKey := locks.acquire("/tmp/a/file.txt")

	assert.Equal(t, "/tmp/a/file.txt", key)
	assert.Equal(t, key, sameKey)
	assert.Same(t, first, second, "cleaned paths must share one lock")
	assert.Equal(t, 2, first.refs)

	locks.release(key)
	locks.release(key)

	assert.Empty(t, locks.entries, "released paths must not accumulate")
}
