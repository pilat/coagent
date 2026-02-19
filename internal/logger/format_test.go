package logger

import (
	"encoding/json"
	"testing"
)

func TestFormatArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     json.RawMessage
		maxLen   int
		expected string
	}{
		{
			name:     "simple object",
			args:     json.RawMessage(`{"path":".","pattern":"*.go"}`),
			maxLen:   200,
			expected: "path=. pattern=*.go",
		},
		{
			name:     "with numbers",
			args:     json.RawMessage(`{"offset":10,"limit":100}`),
			maxLen:   200,
			expected: "limit=100 offset=10",
		},
		{
			name:     "with booleans",
			args:     json.RawMessage(`{"recursive":true,"hidden":false}`),
			maxLen:   200,
			expected: "hidden=false recursive=true",
		},
		{
			name:     "truncated",
			args:     json.RawMessage(`{"long":"this is a very long string that will be truncated"}`),
			maxLen:   20,
			expected: "long=this is a very …",
		},
		{
			name:     "empty",
			args:     json.RawMessage{},
			maxLen:   200,
			expected: "{}",
		},
		{
			name:     "nested array",
			args:     json.RawMessage(`{"files":["a.go","b.go"]}`),
			maxLen:   200,
			expected: "files=[\"a.go\",\"b.go\"]",
		},
		{
			name: "long value truncated",
			args: json.RawMessage(
				`{"content":"this is a very long content that exceeds fifty characters for sure"}`,
			),
			maxLen:   200,
			expected: "content=this is a very long content that exceeds fifty cha…",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatArgs(tt.args, tt.maxLen)
			if result != tt.expected {
				t.Errorf("FormatArgs() = %q, want %q", result, tt.expected)
			}
		})
	}
}
