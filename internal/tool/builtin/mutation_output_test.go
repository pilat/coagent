package builtin

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const truncationNotice = "\n... output truncated"

func TestBoundedMutationOutputTruncationBoundary(t *testing.T) {
	tests := []struct {
		name  string
		write int
		want  string
	}{
		{
			name:  "exactly at the limit is kept whole",
			write: fileMutationOutputLimit,
			want:  strings.Repeat("x", fileMutationOutputLimit),
		},
		{
			name:  "one byte over is cut and flagged",
			write: fileMutationOutputLimit + 1,
			want:  strings.Repeat("x", fileMutationOutputLimit) + truncationNotice,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out boundedMutationOutput

			n, err := out.Write([]byte(strings.Repeat("x", tt.write)))
			require.NoError(t, err)

			assert.Equal(t, tt.write, n, "the writer must claim every byte it was handed")
			assert.Equal(t, tt.want, out.String())
		})
	}
}

// The limit is spent across writes, and the kept prefix must survive the notice.
func TestBoundedMutationOutputAccumulatesAcrossWrites(t *testing.T) {
	var out boundedMutationOutput

	chunk := strings.Repeat("a", fileMutationOutputLimit/2)

	for range 3 {
		n, err := out.Write([]byte(chunk))
		require.NoError(t, err)
		assert.Equal(t, len(chunk), n)
	}

	result := out.String()

	assert.True(t, strings.HasPrefix(result, chunk), "the first chunk must still be readable")
	assert.Len(t, strings.TrimSuffix(result, truncationNotice), fileMutationOutputLimit)
	assert.True(t, strings.HasSuffix(result, truncationNotice))
}

func TestBoundedMutationOutputDiscardsWritesPastTheLimit(t *testing.T) {
	var out boundedMutationOutput

	_, err := out.Write([]byte(strings.Repeat("x", fileMutationOutputLimit)))
	require.NoError(t, err)

	n, err := out.Write([]byte("dropped"))
	require.NoError(t, err)

	assert.Equal(t, len("dropped"), n)
	assert.Equal(t, strings.Repeat("x", fileMutationOutputLimit)+truncationNotice, out.String())
}
