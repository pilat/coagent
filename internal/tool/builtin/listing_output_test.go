package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/humanize"
	"github.com/pilat/coagent/internal/tool"
)

func TestLsToolOrdersDirectoriesFirstThenNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"zeta", "alpha"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0o755))
	}

	for _, name := range []string{"zeta.txt", "alpha.txt", ".hidden"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("hello"), 0o644))
	}

	result := runLs(t, dir, dir)

	assert.Equal(t, "alpha/\nzeta/\nalpha.txt (5B)\nzeta.txt (5B)", result.Output)
	assert.Equal(t, 4, result.Metadata[metaKeyCount], "hidden entries are excluded from the count")
}

func TestLsToolEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	result := runLs(t, dir, dir)

	assert.Equal(t, "(empty directory)", result.Output)
	assert.Equal(t, 0, result.Metadata[metaKeyCount])
}

func TestLsToolTitleFallsBackToAbsolutePath(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		workDir string
	}{
		{name: "no work dir", workDir: ""},
		{name: "work dir the path cannot be relative to", workDir: "not/absolute"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, dir, runLs(t, tt.workDir, dir).Title)
		})
	}
}

func TestLsToolTitleIsRelativeToWorkDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "nested")
	require.NoError(t, os.Mkdir(sub, 0o755))

	assert.Equal(t, "nested", runLs(t, dir, sub).Title)
}

func TestLsToolRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	lsTool := NewLsTool(dir)

	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "malformed json", raw: `{`, wantErr: "invalid parameters"},
		{name: "missing path", raw: `{"path":"nope"}`, wantErr: "path not found"},
		{name: "path is a file", raw: `{"path":"file.txt"}`, wantErr: "not a directory"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := lsTool.Execute(context.Background(), json.RawMessage(tt.raw))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestLsToolRendersHumanSizes(t *testing.T) {
	assert.Equal(t, "1.0KB", humanize.FormatSize(1024), "tool output sizes flow through humanize.FormatSize")
}

func TestGlobToolExcludesDirectories(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644))

	result := runGlob(t, dir, globParams{Pattern: "*"})

	assert.Equal(t, 1, result.Metadata[metaKeyCount])
	assert.Equal(t, filepath.Join(dir, "file.txt"), result.Output)
}

func TestGlobToolSortsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)

	names := []string{"old.txt", "newest.txt", "middle.txt"}
	offsets := []time.Duration{0, 2 * time.Minute, time.Minute}

	for i, name := range names {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
		require.NoError(t, os.Chtimes(path, base.Add(offsets[i]), base.Add(offsets[i])))
	}

	result := runGlob(t, dir, globParams{Pattern: "*.txt"})

	want := filepath.Join(dir, "newest.txt") + "\n" +
		filepath.Join(dir, "middle.txt") + "\n" +
		filepath.Join(dir, "old.txt")
	assert.Equal(t, want, result.Output)
}

func TestGlobToolNoMatches(t *testing.T) {
	dir := t.TempDir()
	result := runGlob(t, dir, globParams{Pattern: "*.nope"})

	assert.Equal(t, "No files found", result.Output)
	assert.Equal(t, 0, result.Metadata[metaKeyCount])
	assert.Equal(t, false, result.Metadata[metaKeyTruncated])
}

func TestGlobToolRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	globTool := newGlobTool(dir)

	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "malformed json", raw: `{`, wantErr: "invalid parameters"},
		{name: "empty pattern", raw: `{"pattern":""}`, wantErr: "pattern is required"},
		{name: "missing path", raw: `{"pattern":"*","path":"nope"}`, wantErr: "path not found"},
		{name: "path is a file", raw: `{"pattern":"*","path":"file.txt"}`, wantErr: "not a directory"},
		{name: "invalid pattern", raw: `{"pattern":"[","path":"."}`, wantErr: "invalid glob pattern"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := globTool.Execute(context.Background(), json.RawMessage(tt.raw))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func runLs(t *testing.T, workDir, path string) *tool.Result {
	t.Helper()

	raw, err := json.Marshal(LsParams{Path: path})
	require.NoError(t, err)

	result, err := NewLsTool(workDir).Execute(context.Background(), raw)
	require.NoError(t, err)

	return result
}

func runGlob(t *testing.T, dir string, p globParams) *tool.Result {
	t.Helper()

	raw, err := json.Marshal(p)
	require.NoError(t, err)

	result, err := newGlobTool(dir).Execute(context.Background(), raw)
	require.NoError(t, err)

	return result
}

func TestGlobToolTruncationBoundary(t *testing.T) {
	tests := []struct {
		name      string
		files     int
		wantCount int
		truncated bool
	}{
		{name: "exactly at the limit", files: globLimit, wantCount: globLimit, truncated: false},
		{name: "one over the limit", files: globLimit + 1, wantCount: globLimit, truncated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for i := range tt.files {
				name := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
				require.NoError(t, os.WriteFile(name, []byte("x"), 0o644))
			}

			result := runGlob(t, dir, globParams{Pattern: "*.txt"})

			assert.Equal(t, tt.wantCount, result.Metadata[metaKeyCount])
			assert.Equal(t, tt.truncated, result.Metadata[metaKeyTruncated])
			assert.Equal(t, tt.truncated, strings.Contains(result.Output, "Results are truncated"))
		})
	}
}

func TestGlobToolTitle(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "nested")
	require.NoError(t, os.Mkdir(sub, 0o755))

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
			raw, err := json.Marshal(globParams{Pattern: "*", Path: tt.path})
			require.NoError(t, err)

			result, err := newGlobTool(tt.workDir).Execute(context.Background(), raw)
			require.NoError(t, err)

			assert.Equal(t, tt.want, result.Title)
		})
	}
}

// A directory sorting ahead of the files must be skipped, not end the scan.
func TestGlobToolSkipsDirectoriesWithoutEndingTheScan(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "a-dir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b-file.txt"), []byte("x"), 0o644))

	result := runGlob(t, dir, globParams{Pattern: "*"})

	assert.Equal(t, 1, result.Metadata[metaKeyCount])
	assert.Equal(t, filepath.Join(dir, "b-file.txt"), result.Output)
}
