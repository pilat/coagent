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

func TestFormatContextPreviewWindowBounds(t *testing.T) {
	lines := previewLines(30)

	tests := []struct {
		name   string
		region editRange
		want   string
	}{
		{
			name:   "replacement spans five lines each side",
			region: editRange{finalStart: 10, finalEnd: 10},
			want:   previewBlock(lines, 5, 15),
		},
		{
			name:   "deletion trails four lines instead of five",
			region: editRange{finalStart: 10, finalEnd: 9},
			want:   previewBlock(lines, 5, 14),
		},
		{
			name:   "window clamps to the first line",
			region: editRange{finalStart: 2, finalEnd: 2},
			want:   previewBlock(lines, 1, 7),
		},
		{
			name:   "window clamps to the last line",
			region: editRange{finalStart: 29, finalEnd: 29},
			want:   previewBlock(lines, 24, 30),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatContextPreview(lines, []editRange{tt.region}))
		})
	}
}

func TestFormatContextPreviewMergesWithinTenLines(t *testing.T) {
	lines := previewLines(60)

	tests := []struct {
		name    string
		regions []editRange
		want    string
	}{
		{
			name:    "gap exactly ten lines merges",
			regions: []editRange{{finalStart: 6, finalEnd: 6}, {finalStart: 26, finalEnd: 26}},
			want:    previewBlock(lines, 1, 31),
		},
		{
			name:    "gap one line wider stays split",
			regions: []editRange{{finalStart: 6, finalEnd: 6}, {finalStart: 27, finalEnd: 27}},
			want:    previewBlock(lines, 1, 11) + previewBlock(lines, 22, 32),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatContextPreview(lines, tt.regions))
		})
	}
}

// A window swallowed by its predecessor must not shrink the merged range.
func TestFormatContextPreviewKeepsWidestMergedEnd(t *testing.T) {
	lines := previewLines(60)
	regions := []editRange{{finalStart: 10, finalEnd: 40}, {finalStart: 20, finalEnd: 20}}

	assert.Equal(t, previewBlock(lines, 5, 45), formatContextPreview(lines, regions))
}

func TestFormatContextPreviewSortsUnorderedRegions(t *testing.T) {
	lines := previewLines(60)
	regions := []editRange{{finalStart: 45, finalEnd: 45}, {finalStart: 10, finalEnd: 10}}

	assert.Equal(t, previewBlock(lines, 5, 15)+previewBlock(lines, 40, 50), formatContextPreview(lines, regions))
}

func TestTieredReplaceAllPreviewTiers(t *testing.T) {
	lines := previewLines(200)

	tests := []struct {
		name        string
		count       int
		wantHeaders int
		wantPrefix  string
		wantSuffix  string
	}{
		{name: "three regions render in full", count: 3, wantHeaders: 3},
		{name: "four regions summarise the tail", count: 4, wantHeaders: 3, wantSuffix: "\n... and 1 more replacement"},
		{
			name:        "five regions pluralise the tail",
			count:       5,
			wantHeaders: 3,
			wantSuffix:  "\n... and 2 more replacements",
		},
		{name: "nine regions still show three", count: 9, wantHeaders: 3, wantSuffix: "\n... and 6 more replacements"},
		{name: "ten regions collapse to one", count: 10, wantHeaders: 1, wantPrefix: "Replaced 10 occurrences.\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			regions := make([]editRange, 0, tt.count)
			for i := range tt.count {
				line := 10 + i*30
				regions = append(regions, editRange{finalStart: line, finalEnd: line})
			}

			out := tieredReplaceAllPreview(lines, regions)

			assert.Equal(t, tt.wantHeaders, strings.Count(out, "Result around edit"))
			assert.True(t, strings.HasPrefix(out, tt.wantPrefix), "output %q", out)
			assert.True(t, strings.HasSuffix(out, tt.wantSuffix), "output %q", out)
		})
	}
}

func TestEditToolKeepsSuccessLineBeforePreview(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "src.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha\nbeta\nalpha\n"), 0o644))

	tests := []struct {
		name   string
		params editParams
	}{
		{name: "single match", params: editParams{FilePath: path, OldString: "beta", NewString: "gamma"}},
		{
			name:   "replace all",
			params: editParams{FilePath: path, OldString: "alpha", NewString: "delta", ReplaceAll: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, []byte("alpha\nbeta\nalpha\n"), 0o644))

			raw, err := json.Marshal(tt.params)
			require.NoError(t, err)

			result, err := newEditTool(dir, nil, directFileMutator{}).Execute(context.Background(), raw)
			require.NoError(t, err)

			assert.True(t, strings.HasPrefix(result.Output, "Edit applied successfully.\n"), "output %q", result.Output)
			assert.Contains(t, result.Output, "Result around edit")
		})
	}
}

func previewLines(n int) []string {
	lines := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		lines = append(lines, fmt.Sprintf("l%d", i))
	}

	return lines
}

func previewBlock(lines []string, start, end int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "\nResult around edit (lines %d-%d):\n", start, end)

	for i := start; i <= end; i++ {
		fmt.Fprintf(&sb, "%d| %s\n", i, lines[i-1])
	}

	return sb.String()
}
