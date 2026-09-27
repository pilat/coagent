package tool

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsUntrustedOutputSource(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "webfetch", id: "webfetch", want: true},
		{name: "websearch", id: "websearch", want: true},
		{name: "mcp tool", id: "mcp__tavily__tavily_search", want: true},
		{name: "minimal mcp name", id: "mcp__x__y", want: true},
		{name: "local read", id: "read", want: false},
		{name: "bash", id: "bash", want: false},
		{name: "batch", id: "batch", want: false},
		{name: "mcp prefix impostor inside a name", id: "readmcp__x", want: false},
		{name: "empty", id: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsUntrustedOutputSource(tt.id))
		})
	}
}

// The provenance bit is execution-time only: generic JSON serialization must
// omit it so every persisted message and provider payload stays untouched.
func TestResultUntrustedIsNeverSerialized(t *testing.T) {
	data, err := json.Marshal(&Result{Output: "body", Untrusted: true})
	require.NoError(t, err)

	assert.JSONEq(t, `{"output":"body"}`, string(data))

	var round Result
	require.NoError(t, json.Unmarshal(data, &round))
	assert.False(t, round.Untrusted)
}

func TestUntrustedMarkerTokens(t *testing.T) {
	assert.NotEqual(t, UntrustedContentBegin, UntrustedContentEnd)
	assert.Equal(t, "<<<BEGIN_UNTRUSTED_EXTERNAL_DATA>>>", UntrustedContentBegin)
	assert.Equal(t, "<<<END_UNTRUSTED_EXTERNAL_DATA>>>", UntrustedContentEnd)
}
