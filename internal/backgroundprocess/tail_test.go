package backgroundprocess

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractTailPreservesLineOrderAndBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		lines   int
		want    string
	}{
		{name: "terminal newline", content: "a\nb\nc\n", lines: 3, want: "a\nb\nc"},
		{name: "no terminal newline", content: "a\nb\nc", lines: 3, want: "a\nb\nc"},
		{name: "newest suffix", content: "a\nb\nc\n", lines: 2, want: "b\nc"},
		{name: "interior empty line", content: "a\n\nb\n", lines: 3, want: "a\n\nb"},
		{name: "leading empty line", content: "\na\n", lines: 2, want: "\na"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "output")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))

			got, _, ok := ExtractTail(path, tt.lines, 8*1024)
			require.True(t, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExtractTailHandlesLineAcrossReadBlocks(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "output")
	long := strings.Repeat("x", tailBlockSize+137)
	require.NoError(t, os.WriteFile(path, []byte("first\n"+long+"\nlast"), 0o600))

	got, truncated, ok := ExtractTail(path, 3, int64(len(long)+32))
	require.True(t, ok)
	assert.False(t, truncated)
	assert.Equal(t, "first\n"+long+"\nlast", got)
}

func TestScanTailLinesAppliesLineLimitBeforeByteBudget(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "output")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a", 60)+"\nlast"), 0o600))

	file, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })

	lines, truncated, err := ScanTailLines(file, 65, 2, 32, 10)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, lines, 2)
	assert.Equal(t, "aaaaaaaaaa...", string(lines[0]))
	assert.Equal(t, "last", string(lines[1]))
}
