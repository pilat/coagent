package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadToolCountsTotalLinesBeyondLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ten.txt")
	require.NoError(t, os.WriteFile(path, []byte(numberedLines(1, 10)), 0o644))

	params, err := json.Marshal(readParams{FilePath: path, Limit: 3})
	require.NoError(t, err)

	result, err := newReadTool(dir).Execute(context.Background(), params)
	require.NoError(t, err)

	assert.Equal(t, 3, result.Metadata["lines"])
	assert.Equal(t, 10, result.Metadata["total"], "lines past the limit must still be counted")
	assert.Equal(t, true, result.Metadata[metaKeyTruncated])
}

func TestReadToolReportsLastReadLineWithOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ten.txt")
	require.NoError(t, os.WriteFile(path, []byte(numberedLines(1, 10)), 0o644))

	params, err := json.Marshal(readParams{FilePath: path, Offset: 2, Limit: 3})
	require.NoError(t, err)

	result, err := newReadTool(dir).Execute(context.Background(), params)
	require.NoError(t, err)

	assert.Contains(t, result.Output, "3| line 3")
	assert.Contains(t, result.Output, "5| line 5")
	assert.Contains(t, result.Output, "read beyond line 5")
}

func TestReadToolTruncatesLinesExactlyAtMaxLineLength(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name      string
		lineLen   int
		wantLen   int
		wantSplit bool
	}{
		{name: "exactly at limit", lineLen: maxLineLength, wantLen: maxLineLength, wantSplit: false},
		{name: "one over limit", lineLen: maxLineLength + 1, wantLen: maxLineLength, wantSplit: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name+".txt")
			require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", tt.lineLen)+"\n"), 0o644))

			params, err := json.Marshal(readParams{FilePath: path})
			require.NoError(t, err)

			result, err := newReadTool(dir).Execute(context.Background(), params)
			require.NoError(t, err)

			body := strings.TrimPrefix(strings.SplitN(result.Output, "\n", 2)[1], "1| ")
			line := strings.SplitN(body, "\n", 2)[0]

			assert.Equal(t, tt.wantSplit, strings.HasSuffix(line, "..."))
			assert.Len(t, strings.TrimSuffix(line, "..."), tt.wantLen)
		})
	}
}

// The byte budget is spent by accumulation, so the stop line pins both the per-line
// cost (content + newline) and the running total.
func TestReadToolStopsAtByteBudget(t *testing.T) {
	const (
		lineLen   = 99
		wantLines = maxBytes / (lineLen + 1)
		totalLine = wantLines + 100
	)

	dir := t.TempDir()
	path := filepath.Join(dir, "wide.txt")

	var sb strings.Builder
	for range totalLine {
		sb.WriteString(strings.Repeat("y", lineLen))
		sb.WriteString("\n")
	}

	require.NoError(t, os.WriteFile(path, []byte(sb.String()), 0o644))

	params, err := json.Marshal(readParams{FilePath: path})
	require.NoError(t, err)

	result, err := newReadTool(dir).Execute(context.Background(), params)
	require.NoError(t, err)

	assert.Equal(t, wantLines, result.Metadata["lines"])
	assert.Equal(t, totalLine, result.Metadata["total"], "the tail must still be counted after the byte cut")
	assert.Contains(t, result.Output, fmt.Sprintf("truncated at %d bytes", maxBytes))
	assert.Contains(t, result.Output, fmt.Sprintf("read beyond line %d", wantLines))
}

func TestIsBinaryFileByExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.zip")
	require.NoError(t, os.WriteFile(path, []byte("plain text, binary extension"), 0o644))

	assert.True(t, isBinaryFile(path))
}

func TestIsBinaryFileByContent(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		content []byte
		want    bool
	}{
		{name: "null byte anywhere", content: append([]byte("text"), 0x00), want: true},
		{name: "empty file", content: nil, want: false},
		{name: "plain text", content: []byte(strings.Repeat("abc\n", 100)), want: false},
		{name: "tabs are printable", content: mixedBytes('\t', 40), want: false},
		{name: "carriage returns are printable", content: mixedBytes('\r', 40), want: false},
		{name: "spaces are printable", content: mixedBytes(' ', 40), want: false},
		{name: "control bytes exactly at ratio", content: mixedBytes(0x01, 30), want: false},
		{name: "control bytes over ratio", content: mixedBytes(0x01, 31), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "_")+".txt")
			require.NoError(t, os.WriteFile(path, tt.content, 0o644))

			assert.Equal(t, tt.want, isBinaryFile(path))
		})
	}
}

func TestReadToolRejectsBinaryContentUnderTextExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(path, mixedBytes(0x01, 50), 0o644))

	params, err := json.Marshal(readParams{FilePath: path})
	require.NoError(t, err)

	_, err = newReadTool(dir).Execute(context.Background(), params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary")
}

// mixedBytes builds a 100-byte buffer holding n copies of b padded with letters,
// so n is directly the non-printable percentage isBinaryFile measures.
func mixedBytes(b byte, n int) []byte {
	buf := make([]byte, 0, 100)
	for range n {
		buf = append(buf, b)
	}

	for range 100 - n {
		buf = append(buf, 'a')
	}

	return buf
}

func numberedLines(from, to int) string {
	var sb strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}

	return sb.String()
}

func TestReadToolMarksFullReadAsComplete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ten.txt")
	require.NoError(t, os.WriteFile(path, []byte(numberedLines(1, 10)), 0o644))

	params, err := json.Marshal(readParams{FilePath: path, Limit: 10})
	require.NoError(t, err)

	result, err := newReadTool(dir).Execute(context.Background(), params)
	require.NoError(t, err)

	assert.Equal(t, false, result.Metadata[metaKeyTruncated])
	assert.Contains(t, result.Output, "(End of file - total 10 lines)")
	assert.NotContains(t, result.Output, "more lines")
}

func TestRelativeTitle(t *testing.T) {
	tests := []struct {
		name     string
		workDir  string
		filePath string
		want     string
	}{
		{name: "no work dir", workDir: "", filePath: "/a/b/c.txt", want: "/a/b/c.txt"},
		{name: "nested under work dir", workDir: "/a", filePath: "/a/b/c.txt", want: "b/c.txt"},
		{name: "work dir it cannot be relative to", workDir: "relative", filePath: "/a/b/c.txt", want: "/a/b/c.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, relativeTitle(tt.workDir, tt.filePath))
		})
	}
}

func TestNormalizeReadBounds(t *testing.T) {
	tests := []struct {
		name       string
		offset     int
		limit      int
		wantOffset int
		wantLimit  int
	}{
		{name: "negative offset floors at zero", offset: -5, limit: 10, wantOffset: 0, wantLimit: 10},
		{name: "zero limit falls back to the default", offset: 3, limit: 0, wantOffset: 3, wantLimit: defaultReadLimit},
		{
			name:       "negative limit falls back to the default",
			offset:     0,
			limit:      -1,
			wantOffset: 0,
			wantLimit:  defaultReadLimit,
		},
		{name: "explicit values pass through", offset: 4, limit: 7, wantOffset: 4, wantLimit: 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offset, limit := normalizeReadBounds(tt.offset, tt.limit)
			assert.Equal(t, tt.wantOffset, offset)
			assert.Equal(t, tt.wantLimit, limit)
		})
	}
}

// The byte budget stops the read at the first line that does not fit; later
// lines are not cherry-picked just because they are shorter.
func TestReadToolStopsAtFirstOversizedLine(t *testing.T) {
	const (
		fillLines  = 511
		fillLen    = 99
		bigLen     = 200
		shortMaker = "SHORTLINE"
	)

	dir := t.TempDir()
	path := filepath.Join(dir, "mixed.txt")

	var sb strings.Builder
	for range fillLines {
		sb.WriteString(strings.Repeat("y", fillLen))
		sb.WriteString("\n")
	}

	sb.WriteString(strings.Repeat("Y", bigLen))
	sb.WriteString("\n")
	sb.WriteString(shortMaker)
	sb.WriteString("\n")

	require.NoError(t, os.WriteFile(path, []byte(sb.String()), 0o644))

	params, err := json.Marshal(readParams{FilePath: path})
	require.NoError(t, err)

	result, err := newReadTool(dir).Execute(context.Background(), params)
	require.NoError(t, err)

	assert.Equal(t, fillLines, result.Metadata["lines"])
	assert.Equal(t, fillLines+2, result.Metadata["total"])
	assert.NotContains(t, result.Output, shortMaker)
}
