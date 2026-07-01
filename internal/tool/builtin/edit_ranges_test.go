package builtin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecuteStrReplaceRanges(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		oldString  string
		newString  string
		wantResult string
		wantRanges []editRange
	}{
		{
			name:       "single line replacement",
			content:    "one\ntwo\nthree\n",
			oldString:  "two",
			newString:  "TWO",
			wantResult: "one\nTWO\nthree\n",
			wantRanges: []editRange{{finalStart: 2, finalEnd: 2}},
		},
		{
			name:       "replacement grows by two lines",
			content:    "one\ntwo\nthree\n",
			oldString:  "two",
			newString:  "a\nb\nc",
			wantResult: "one\na\nb\nc\nthree\n",
			wantRanges: []editRange{{finalStart: 2, finalEnd: 4}},
		},
		{
			name:       "deletion collapses to one line",
			content:    "one\ntwo\nthree\n",
			oldString:  "two\n",
			newString:  "",
			wantResult: "one\nthree\n",
			wantRanges: []editRange{{finalStart: 2, finalEnd: 2}},
		},
		{
			name:       "match on the first line",
			content:    "one\ntwo\n",
			oldString:  "one",
			newString:  "1",
			wantResult: "1\ntwo\n",
			wantRanges: []editRange{{finalStart: 1, finalEnd: 1}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ranges, err := executeStrReplace(tt.content, tt.oldString, tt.newString)
			require.NoError(t, err)

			assert.Equal(t, tt.wantResult, got)
			assert.Equal(t, tt.wantRanges, ranges)
		})
	}
}

func TestExecuteStrReplaceRejections(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		oldString string
		newString string
		wantErr   string
	}{
		{name: "identical strings", content: "a", oldString: "a", newString: "a", wantErr: "must be different"},
		{name: "no match", content: "a", oldString: "z", newString: "y", wantErr: "not found in file"},
		{
			name:      "ambiguous match",
			content:   "a\na\na\n",
			oldString: "a",
			newString: "b",
			wantErr:   "oldString found 3 times in file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := executeStrReplace(tt.content, tt.oldString, tt.newString)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// Positions of later occurrences shift by the accumulated length delta, so the
// ranges must be computed against the rewritten content, not the original.
func TestExecuteReplaceAllRangesAccountForDrift(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		oldString  string
		newString  string
		wantResult string
		wantRanges []editRange
	}{
		{
			name:       "growing multi-line replacement",
			content:    "x\nA\ny\nA\nz\n",
			oldString:  "A",
			newString:  "B\nC",
			wantResult: "x\nB\nC\ny\nB\nC\nz\n",
			wantRanges: []editRange{{finalStart: 2, finalEnd: 3}, {finalStart: 5, finalEnd: 6}},
		},
		{
			name:       "shrinking replacement",
			content:    "long\nmid\nlong\n",
			oldString:  "long",
			newString:  "s",
			wantResult: "s\nmid\ns\n",
			wantRanges: []editRange{{finalStart: 1, finalEnd: 1}, {finalStart: 3, finalEnd: 3}},
		},
		{
			name:       "three occurrences on one line",
			content:    "aXaXa\n",
			oldString:  "X",
			newString:  "YY",
			wantResult: "aYYaYYa\n",
			wantRanges: []editRange{{finalStart: 1, finalEnd: 1}, {finalStart: 1, finalEnd: 1}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ranges, err := executeReplaceAll(tt.content, tt.oldString, tt.newString)
			require.NoError(t, err)

			assert.Equal(t, tt.wantResult, got)
			assert.Equal(t, tt.wantRanges, ranges)
		})
	}
}

func TestExecuteReplaceAllRejections(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		oldString string
		newString string
		wantErr   string
	}{
		{name: "empty old string", content: "a", oldString: "", newString: "b", wantErr: "oldString is required"},
		{name: "identical strings", content: "a", oldString: "a", newString: "a", wantErr: "must be different"},
		{name: "no match", content: "a", oldString: "z", newString: "y", wantErr: "not found in file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := executeReplaceAll(tt.content, tt.oldString, tt.newString)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
