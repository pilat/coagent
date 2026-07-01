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

	"github.com/pilat/coagent/internal/tool"
)

func TestBuildGrepOutputShape(t *testing.T) {
	matches := []grepMatch{
		{file: "a.go", line: 1, content: "alpha"},
		{file: "a.go", line: 3, content: "alpha again"},
		{file: "b.go", line: 2, content: "alpha too"},
	}

	tests := []struct {
		name          string
		filesOnly     bool
		matches       []grepMatch
		matchingFiles []string
		truncated     bool
		want          string
	}{
		{
			name:          "one header per file, blank line only between groups",
			matches:       matches,
			matchingFiles: []string{"a.go", "b.go"},
			want:          "=== a.go ===\n1: alpha\n3: alpha again\n\n=== b.go ===\n2: alpha too\n",
		},
		{
			name:          "context lines precede their match",
			matches:       []grepMatch{{file: "a.go", line: 3, content: "hit", context: []string{"1- one", "2- two"}}},
			matchingFiles: []string{"a.go"},
			want:          "=== a.go ===\n1- one\n2- two\n3: hit\n",
		},
		{
			name:          "truncation notice is appended once",
			matches:       matches[:1],
			matchingFiles: []string{"a.go"},
			truncated:     true,
			want:          "=== a.go ===\n1: alpha\n\n(Results truncated at 100 matches)",
		},
		{
			name: "no matches",
			want: "No matches found",
		},
		{
			name:          "files only lists paths",
			filesOnly:     true,
			matches:       matches,
			matchingFiles: []string{"a.go", "b.go"},
			want:          "a.go\nb.go\n",
		},
		{
			name:      "files only with nothing found",
			filesOnly: true,
			want:      "No files matched",
		},
		{
			name:          "files only ignores the truncation notice",
			filesOnly:     true,
			matches:       matches,
			matchingFiles: []string{"a.go"},
			truncated:     true,
			want:          "a.go\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, buildGrepOutput(tt.filesOnly, tt.matches, tt.matchingFiles, tt.truncated))
		})
	}
}

func TestGrepToolCapsMatchesPerFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "many.txt"), []byte(repeatLine("needle", 25)), 0o644))

	result := runGrep(t, dir, grepParams{Pattern: "needle"})

	assert.Equal(t, grepMaxMatchesFile, result.Metadata["matches"])
	assert.Equal(t, 1, result.Metadata["files"])
	assert.Equal(t, false, result.Metadata[metaKeyTruncated])
}

func TestGrepToolCapsTotalMatches(t *testing.T) {
	dir := t.TempDir()
	for i := range 6 {
		name := fmt.Sprintf("f%d.txt", i)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(repeatLine("needle", 20)), 0o644))
	}

	result := runGrep(t, dir, grepParams{Pattern: "needle"})

	assert.Equal(t, grepMaxMatches, result.Metadata["matches"])
	assert.Equal(t, grepMaxMatches/grepMaxMatchesFile, result.Metadata["files"], "the last file must not be opened")
	assert.Equal(t, true, result.Metadata[metaKeyTruncated])
	assert.Contains(t, result.Output, "(Results truncated at 100 matches)")
}

func TestGrepToolSkipsOversizedAndBinaryFiles(t *testing.T) {
	dir := t.TempDir()

	oversized := strings.Repeat("needle padding\n", grepMaxFileSize/15+1)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.txt"), []byte(oversized), 0o644))

	blob := append([]byte("needle\n"), mixedBytes(0x01, 60)...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blob.txt"), blob, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("needle\n"), 0o644))

	result := runGrep(t, dir, grepParams{Pattern: "needle", FilesOnly: true})

	assert.Equal(t, 1, result.Metadata["files"])
	assert.Contains(t, result.Output, "ok.txt")
	assert.NotContains(t, result.Output, "big.txt")
	assert.NotContains(t, result.Output, "blob.txt")
}

func TestGrepToolIgnoreCaseIsOptIn(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "case.txt"), []byte("Needle\n"), 0o644))

	assert.Equal(t, 0, runGrep(t, dir, grepParams{Pattern: "needle"}).Metadata["matches"])
	assert.Equal(t, 1, runGrep(t, dir, grepParams{Pattern: "needle", IgnoreCase: true}).Metadata["matches"])
}

func TestGrepToolContextWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ctx.txt")
	require.NoError(t, os.WriteFile(path, []byte("one\ntwo\nneedle\n"), 0o644))

	tests := []struct {
		name    string
		context int
		want    string
	}{
		{name: "no context", context: 0, want: "3: needle"},
		{name: "one leading line", context: 1, want: "2- two\n3: needle"},
		{name: "more context than the file has", context: 9, want: "1- one\n2- two\n3: needle"},
		{name: "negative context is clamped", context: -3, want: "3: needle"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runGrep(t, dir, grepParams{Pattern: "needle", Context: tt.context})
			assert.Equal(t, "=== "+path+" ===\n"+tt.want, result.Output)
		})
	}
}

func TestGrepToolRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	grepTool := newGrepTool(dir)

	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "malformed json", raw: `{`, wantErr: "invalid parameters"},
		{name: "empty pattern", raw: `{"pattern":""}`, wantErr: "pattern is required"},
		{name: "invalid regex", raw: `{"pattern":"("}`, wantErr: "invalid regex pattern"},
		{name: "missing path", raw: `{"pattern":"x","path":"nope"}`, wantErr: "path not found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := grepTool.Execute(context.Background(), json.RawMessage(tt.raw))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func runGrep(t *testing.T, dir string, p grepParams) *tool.Result {
	t.Helper()

	raw, err := json.Marshal(p)
	require.NoError(t, err)

	result, err := newGrepTool(dir).Execute(context.Background(), raw)
	require.NoError(t, err)

	return result
}

func repeatLine(text string, n int) string {
	var sb strings.Builder
	for range n {
		sb.WriteString(text)
		sb.WriteString("\n")
	}

	return sb.String()
}

// grepMaxFileSize is a skip threshold, not a read budget: a file sitting exactly
// on it is still searched.
func TestGrepToolSearchesFileExactlyAtSizeLimit(t *testing.T) {
	dir := t.TempDir()

	body := "needle\n" + strings.Repeat("p", grepMaxFileSize-len("needle\n"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "edge.txt"), []byte(body), 0o644))

	assert.Equal(t, 1, runGrep(t, dir, grepParams{Pattern: "needle"}).Metadata["matches"])
}

// The per-file cap can carry the running total past the global cap mid-file; the
// inner guard has to stop on the exact hundredth match.
func TestGrepToolStopsMidFileAtGlobalCap(t *testing.T) {
	dir := t.TempDir()

	counts := map[string]int{"a.txt": 20, "b.txt": 20, "c.txt": 20, "d.txt": 20, "e.txt": 5, "f.txt": 20}
	for name, n := range counts {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(repeatLine("needle", n)), 0o644))
	}

	result := runGrep(t, dir, grepParams{Pattern: "needle"})

	assert.Equal(t, grepMaxMatches, result.Metadata["matches"])
	assert.Equal(t, 6, result.Metadata["files"])
	assert.Equal(t, true, result.Metadata[metaKeyTruncated])
}

func TestGrepToolTitle(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "nested")
	require.NoError(t, os.Mkdir(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "f.txt"), []byte("needle\n"), 0o644))

	tests := []struct {
		name    string
		workDir string
		path    string
		want    string
	}{
		{name: "relative to work dir", workDir: dir, path: sub, want: "nested"},
		{name: "no work dir", workDir: "", path: sub, want: sub},
		{name: "work dir it cannot be relative to", workDir: "relative", path: sub, want: sub},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(grepParams{Pattern: "needle", Path: tt.path})
			require.NoError(t, err)

			result, err := newGrepTool(tt.workDir).Execute(context.Background(), raw)
			require.NoError(t, err)

			assert.Equal(t, tt.want, result.Title)
		})
	}
}

func TestGrepToolSearchesASingleFilePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "one.txt")
	require.NoError(t, os.WriteFile(path, []byte("needle\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "two.txt"), []byte("needle\n"), 0o644))

	result := runGrep(t, dir, grepParams{Pattern: "needle", Path: path})

	assert.Equal(t, 1, result.Metadata["files"])
	assert.Contains(t, result.Output, "one.txt")
	assert.NotContains(t, result.Output, "two.txt")
}

// A dangling symlink cannot be stat'ed; grep must step over it, not fail on it.
func TestGrepToolSkipsUnstattablePaths(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "a-link")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b-file.txt"), []byte("needle\n"), 0o644))

	result := runGrep(t, dir, grepParams{Pattern: "needle", FilesOnly: true})

	assert.Equal(t, 1, result.Metadata["files"])
	assert.Equal(t, filepath.Join(dir, "b-file.txt"), result.Output)
}
