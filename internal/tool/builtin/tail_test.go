package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

func TestTailToolPreservesOrderAndEmptyLines(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "output")
	require.NoError(t, os.WriteFile(path, []byte("a\n\nb\nc\n"), 0o600))

	result, err := newTailTool(dir, nil).Execute(
		context.Background(), json.RawMessage(`{"file_path":"output","lines":4}`),
	)
	require.NoError(t, err)
	assert.Equal(t, "a\n\nb\nc", result.Output)
}

func TestTailToolTruncatesLongLinesWithinTotalLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "output")
	content := strings.Repeat("x", maxLineLength+500) + "\nlast"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	result, err := newTailTool(dir, nil).Execute(
		context.Background(), json.RawMessage(`{"file_path":"output","lines":2}`),
	)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("x", maxLineLength)+"...\nlast", result.Output)
	assert.LessOrEqual(t, len(result.Output), tailMaxBytes)
}

func TestTailToolTruncatesLongMultibyteLineAtRuneBoundary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "output")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("界", maxLineLength)), 0o600))

	result, err := newTailTool(dir, nil).Execute(
		context.Background(), json.RawMessage(`{"file_path":"output","lines":1}`),
	)
	require.NoError(t, err)
	assert.True(t, utf8.ValidString(result.Output))
	assert.True(t, strings.HasSuffix(result.Output, "..."))
}

func TestTailToolRejectsNonpositiveLines(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "output")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))

	for _, lines := range []int{0, -1} {
		_, err := newTailTool(dir, nil).Execute(
			context.Background(),
			json.RawMessage(fmt.Sprintf(`{"file_path":"output","lines":%d}`, lines)),
		)
		require.ErrorContains(t, err, "lines must be positive")
	}
}

func TestTailToolCapsVisibleOutput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "output")
	content := strings.Repeat(strings.Repeat("x", 1000)+"\n", 100)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	result, err := newTailTool(dir, nil).Execute(
		context.Background(), json.RawMessage(`{"file_path":"output","lines":2000}`),
	)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(result.Output), tailMaxBytes)
	assert.Contains(t, result.Output, "tail truncated")
}

func TestTailToolRejectsKnownBinaryExtension(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "printable.pdf")
	require.NoError(t, os.WriteFile(path, []byte("printable prefix"), 0o600))

	_, err := newTailTool(dir, nil).Execute(
		context.Background(), json.RawMessage(`{"file_path":"printable.pdf"}`),
	)
	require.ErrorContains(t, err, "tail requires a text file")
}

func TestTailToolRejectsFIFOWithoutBlocking(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "output")
	require.NoError(t, syscall.Mkfifo(path, 0o600))

	done := make(chan error, 1)
	go func() {
		_, err := newTailTool(dir, nil).Execute(
			context.Background(), json.RawMessage(`{"file_path":"output"}`),
		)
		done <- err
	}()

	select {
	case err := <-done:
		require.ErrorContains(t, err, "tail requires a regular file")
	case <-time.After(time.Second):
		t.Fatal("tail blocked opening a FIFO")
	}
}

func TestTailToolSerializesWithFileMutation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "output")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o600))

	unlock := lockFileWrite(path)
	var once sync.Once
	t.Cleanup(func() { once.Do(unlock) })

	done := make(chan *tool.Result, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := newTailTool(dir, nil).Execute(
			context.Background(), json.RawMessage(`{"file_path":"output"}`),
		)
		done <- result
		errs <- err
	}()

	select {
	case <-done:
		t.Fatal("tail crossed an in-flight file mutation")
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, os.WriteFile(path, []byte("after"), 0o600))
	once.Do(unlock)
	result := <-done
	require.NoError(t, <-errs)
	assert.Equal(t, "after", result.Output)
}
