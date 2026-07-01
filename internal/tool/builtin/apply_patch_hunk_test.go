package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePatchHeaders(t *testing.T) {
	tests := []struct {
		name  string
		patch string
		want  []patchFile
	}{
		{
			name:  "counts default to one when omitted",
			patch: "--- a/f.txt\n+++ b/f.txt\n@@ -3 +4 @@\n-old\n+new\n",
			want: []patchFile{{
				Path: "f.txt",
				Hunks: []patchHunk{{
					OldStart: 3, OldCount: 1, NewStart: 4, NewCount: 1,
					Lines: []patchLine{{Type: '-', Content: "old"}, {Type: '+', Content: "new"}},
				}},
			}},
		},
		{
			name:  "explicit counts are kept",
			patch: "--- a/f.txt\n+++ b/f.txt\n@@ -3,2 +4,5 @@\n ctx\n",
			want: []patchFile{{
				Path: "f.txt",
				Hunks: []patchHunk{{
					OldStart: 3, OldCount: 2, NewStart: 4, NewCount: 5,
					Lines: []patchLine{{Type: ' ', Content: "ctx"}},
				}},
			}},
		},
		{
			name:  "two hunks in one file",
			patch: "--- a/f.txt\n+++ b/f.txt\n@@ -1,1 +1,1 @@\n+one\n@@ -5,1 +5,1 @@\n+five\n",
			want: []patchFile{{
				Path: "f.txt",
				Hunks: []patchHunk{
					{
						OldStart: 1,
						OldCount: 1,
						NewStart: 1,
						NewCount: 1,
						Lines:    []patchLine{{Type: '+', Content: "one"}},
					},
					{
						OldStart: 5,
						OldCount: 1,
						NewStart: 5,
						NewCount: 1,
						Lines:    []patchLine{{Type: '+', Content: "five"}},
					},
				},
			}},
		},
		{
			name: "two files are flushed separately",
			patch: "--- a/one.txt\n+++ b/one.txt\n@@ -1,1 +1,1 @@\n+1\n" +
				"--- a/two.txt\n+++ b/two.txt\n@@ -1,1 +1,1 @@\n+2\n",
			want: []patchFile{
				{
					Path: "one.txt",
					Hunks: []patchHunk{
						{
							OldStart: 1,
							OldCount: 1,
							NewStart: 1,
							NewCount: 1,
							Lines:    []patchLine{{Type: '+', Content: "1"}},
						},
					},
				},
				{
					Path: "two.txt",
					Hunks: []patchHunk{
						{
							OldStart: 1,
							OldCount: 1,
							NewStart: 1,
							NewCount: 1,
							Lines:    []patchLine{{Type: '+', Content: "2"}},
						},
					},
				},
			},
		},
		{
			name:  "noise between hunks is dropped",
			patch: "diff --git a/f.txt b/f.txt\n--- a/f.txt\n+++ b/f.txt\n@@ -1,1 +1,1 @@\n+kept\n\\ No newline at end of file\n",
			want: []patchFile{
				{
					Path: "f.txt",
					Hunks: []patchHunk{
						{
							OldStart: 1,
							OldCount: 1,
							NewStart: 1,
							NewCount: 1,
							Lines:    []patchLine{{Type: '+', Content: "kept"}},
						},
					},
				},
			},
		},
		{
			name:  "hunk without a file header is discarded",
			patch: "@@ -1,1 +1,1 @@\n+orphan\n",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePatch(tt.patch)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParsePatchStripsPathPrefixes(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{line: "+++ b/dir/f.txt", want: "dir/f.txt"},
		{line: "+++ a/dir/f.txt", want: "dir/f.txt"},
		{line: "+++ dir/f.txt", want: "dir/f.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			assert.Equal(t, tt.want, parsePatchPath(tt.line))
		})
	}
}

func TestAppendPatchLineFiltersMarkers(t *testing.T) {
	hunk := &patchHunk{}

	for _, line := range []string{"", "\\ No newline", "@ stray", " ctx", "+add", "-del", "x other"} {
		appendPatchLine(hunk, line)
	}

	assert.Equal(t, []patchLine{
		{Type: ' ', Content: "ctx"},
		{Type: '+', Content: "add"},
		{Type: '-', Content: "del"},
	}, hunk.Lines)
}

func TestAppendPatchLineIgnoresNilHunk(t *testing.T) {
	assert.NotPanics(t, func() { appendPatchLine(nil, "+add") })
}

func TestApplyHunkReplacesTargetRange(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}

	tests := []struct {
		name string
		hunk patchHunk
		want []string
	}{
		{
			name: "replace a single line in place",
			hunk: patchHunk{OldStart: 2, OldCount: 1, Lines: []patchLine{{Type: '+', Content: "B"}}},
			want: []string{"a", "B", "c", "d", "e"},
		},
		{
			name: "context lines are kept alongside additions",
			hunk: patchHunk{OldStart: 2, OldCount: 2, Lines: []patchLine{
				{Type: ' ', Content: "b"}, {Type: '+', Content: "b2"}, {Type: '-', Content: "c"},
			}},
			want: []string{"a", "b", "b2", "d", "e"},
		},
		{
			name: "deletion removes the range",
			hunk: patchHunk{
				OldStart: 2,
				OldCount: 2,
				Lines:    []patchLine{{Type: '-', Content: "b"}, {Type: '-', Content: "c"}},
			},
			want: []string{"a", "d", "e"},
		},
		{
			name: "start before the file is clamped to the top",
			hunk: patchHunk{OldStart: 0, OldCount: 1, Lines: []patchLine{{Type: '+', Content: "A"}}},
			want: []string{"A", "b", "c", "d", "e"},
		},
		{
			name: "start past the end appends",
			hunk: patchHunk{OldStart: 99, OldCount: 1, Lines: []patchLine{{Type: '+', Content: "f"}}},
			want: []string{"a", "b", "c", "d", "e", "f"},
		},
		{
			name: "count past the end is clamped",
			hunk: patchHunk{OldStart: 4, OldCount: 4, Lines: []patchLine{{Type: '+', Content: "D"}}},
			want: []string{"a", "b", "c", "D"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, applyHunk(lines, tt.hunk))
		})
	}
}

func TestParsePatchKeepsFileHeaderWithoutHunks(t *testing.T) {
	got, err := parsePatch("--- a/f.txt\n+++ b/f.txt\n")
	require.NoError(t, err)

	assert.Equal(t, []patchFile{{Path: "f.txt"}}, got)
}

// A file header with no hunks is flushed when the next file starts; the flush must
// not try to append the hunk that is not there.
func TestParsePatchFlushesHunklessFileBeforeTheNextOne(t *testing.T) {
	patch := "--- a/one.txt\n+++ b/one.txt\n" +
		"--- a/two.txt\n+++ b/two.txt\n@@ -1,1 +1,1 @@\n+2\n"

	got, err := parsePatch(patch)
	require.NoError(t, err)

	assert.Equal(t, []patchFile{
		{Path: "one.txt"},
		{
			Path: "two.txt",
			Hunks: []patchHunk{{
				OldStart: 1, OldCount: 1, NewStart: 1, NewCount: 1,
				Lines: []patchLine{{Type: '+', Content: "2"}},
			}},
		},
	}, got)
}

// A hunk header is model-written text: it may claim more lines than the file has.
func TestApplyHunkSurvivesOversizedHunkHeader(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}

	tests := []struct {
		name string
		hunk patchHunk
		want []string
	}{
		{
			name: "count far past the end replaces the tail",
			hunk: patchHunk{OldStart: 2, OldCount: 999, Lines: []patchLine{{Type: '+', Content: "B"}}},
			want: []string{"a", "B"},
		},
		{
			name: "count past the end from the first line replaces everything",
			hunk: patchHunk{OldStart: 1, OldCount: 999, Lines: []patchLine{{Type: '+', Content: "only"}}},
			want: []string{"only"},
		},
		{
			name: "start and count both past the end append",
			hunk: patchHunk{OldStart: 99, OldCount: 999, Lines: []patchLine{{Type: '+', Content: "f"}}},
			want: []string{"a", "b", "c", "d", "e", "f"},
		},
		{
			name: "oversized deletion empties the file",
			hunk: patchHunk{OldStart: 1, OldCount: 999},
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, applyHunk(lines, tt.hunk))
		})
	}
}

func TestApplyPatchToolSurvivesOversizedHunkHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "short.txt")
	require.NoError(t, os.WriteFile(path, []byte("one\ntwo\n"), 0o644))

	raw, err := json.Marshal(applyPatchParams{
		Patch: "--- a/short.txt\n+++ b/short.txt\n@@ -1,999 +1,1 @@\n+only\n",
	})
	require.NoError(t, err)

	result, err := newApplyPatchTool(dir, directFileMutator{}).Execute(context.Background(), raw)
	require.NoError(t, err)

	assert.Contains(t, result.Output, "short.txt")

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "only", string(content))
}
