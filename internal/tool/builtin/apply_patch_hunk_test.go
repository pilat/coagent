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
			hunk: patchHunk{
				OldStart: 2,
				OldCount: 1,
				NewCount: 1,
				Lines:    []patchLine{{Type: '-', Content: "b"}, {Type: '+', Content: "B"}},
			},
			want: []string{"a", "B", "c", "d", "e"},
		},
		{
			name: "context lines are kept alongside additions",
			hunk: patchHunk{OldStart: 2, OldCount: 2, NewCount: 3, Lines: []patchLine{
				{
					Type:    ' ',
					Content: "b",
				},
				{Type: '+', Content: "b2"},
				{Type: '-', Content: "c"},
				{Type: '+', Content: "c2"},
			}},
			want: []string{"a", "b", "b2", "c2", "d", "e"},
		},
		{
			name: "deletion removes the range",
			hunk: patchHunk{
				OldStart: 2,
				OldCount: 2,
				NewCount: 0,
				Lines:    []patchLine{{Type: '-', Content: "b"}, {Type: '-', Content: "c"}},
			},
			want: []string{"a", "d", "e"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyHunk(lines, tt.hunk)
			assert.Equal(t, tt.want, got)
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

func TestApplyPatchToolRejectsOversizedHunkHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "short.txt")
	require.NoError(t, os.WriteFile(path, []byte("one\ntwo\n"), 0o644))

	raw, err := json.Marshal(applyPatchParams{
		Patch: "--- a/short.txt\n+++ b/short.txt\n@@ -1,999 +1,1 @@\n+only\n",
	})
	require.NoError(t, err)

	result, err := newApplyPatchTool(dir, directFileMutator{}).Execute(context.Background(), raw)
	require.Error(t, err)
	assert.Nil(t, result)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "one\ntwo\n", string(content))
}

func TestApplyHunkUsesExactAnchorWhenLineHintIsStale(t *testing.T) {
	hunk := patchHunk{
		OldStart: 99,
		OldCount: 1,
		NewCount: 1,
		Lines:    []patchLine{{Type: '-', Content: "target"}, {Type: '+', Content: "updated"}},
	}
	got, _, err := applyHunkAt([]string{"header", "target", "tail"}, hunk, 98, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"header", "updated", "tail"}, got)
}

func TestApplyHunkRejectsMissingOldBlock(t *testing.T) {
	hunk := patchHunk{
		OldStart: 1,
		OldCount: 1,
		NewCount: 1,
		Lines:    []patchLine{{Type: '-', Content: "missing"}, {Type: '+', Content: "new"}},
	}
	_, _, err := applyHunkAt([]string{"present"}, hunk, 0, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "old block not found")
}

func TestApplyHunkFallsBackToTrailingWhitespace(t *testing.T) {
	hunk := patchHunk{
		OldStart: 1,
		OldCount: 1,
		NewCount: 1,
		Lines:    []patchLine{{Type: '-', Content: "target"}, {Type: '+', Content: "updated"}},
	}
	got, _, err := applyHunkAt([]string{"target   "}, hunk, 0, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"updated"}, got)
}

func TestApplyHunkRejectsAmbiguousTie(t *testing.T) {
	hunk := patchHunk{
		OldStart: 2,
		OldCount: 1,
		NewCount: 1,
		Lines:    []patchLine{{Type: '-', Content: "target"}, {Type: '+', Content: "updated"}},
	}
	_, _, err := applyHunkAt([]string{"target", "other", "target"}, hunk, 1, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")
}

func TestApplyHunkPrefersExactRunningContentMetric(t *testing.T) {
	hunk := patchHunk{
		OldStart: 1,
		OldCount: 1,
		NewCount: 1,
		Lines:    []patchLine{{Type: '-', Content: "target"}, {Type: '+', Content: "updated"}},
	}
	got, _, err := applyHunkAt([]string{"target ", "other", "target"}, hunk, 0, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"target ", "other", "updated"}, got)
}

func TestApplyHunkRejectsContextOnMissingFile(t *testing.T) {
	hunk := patchHunk{
		OldStart: 1,
		OldCount: 1,
		NewCount: 2,
		Lines:    []patchLine{{Type: ' ', Content: "context"}, {Type: '+', Content: "new"}},
	}
	_, _, err := applyHunkAt(nil, hunk, 0, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "old block not found")
}
