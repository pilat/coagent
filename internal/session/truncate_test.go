package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTruncateHeadTail_UnderLimit(t *testing.T) {
	input := "short string"
	result := truncateHeadTail(input, 25000)
	assert.Equal(t, input, result)
}

func TestTruncateHeadTail_OverLimit(t *testing.T) {
	input := strings.Repeat("x", 50000)
	result := truncateHeadTail(input, 25000)

	runes := []rune(result)
	require.LessOrEqual(t, len(runes), 25000)
	assert.Contains(t, result, "... (omitted")
	assert.True(t, strings.HasPrefix(result, "xxx"))
	assert.True(t, strings.HasSuffix(result, "xxx"))
}

func TestTruncateHeadTail_Empty(t *testing.T) {
	assert.Empty(t, truncateHeadTail("", 100))
}

func TestTruncateHeadTail_VerySmallLimit(t *testing.T) {
	input := strings.Repeat("x", 100)
	result := truncateHeadTail(input, 5)
	assert.LessOrEqual(t, len([]rune(result)), 5)
}

func TestHasImportantTail_ErrorPatterns(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"error with colon", "some output\nerror: something broke", true},
		{"panic with colon", "normal output\npanic: nil pointer", true},
		{"exit code", "process exited with exit code 1", true},
		{"json closing brace", `{"key": "value"}`, true},
		{"no patterns", "just regular output text here", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Repeat("padding\n", 300) + tt.input
			assert.Equal(t, tt.expected, hasImportantTail(input))
		})
	}
}
