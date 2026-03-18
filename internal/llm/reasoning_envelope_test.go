package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrapAndUnwrapReasoning(t *testing.T) {
	payload := json.RawMessage(`[{"type":"thinking","thinking":"hmm"}]`)

	sealed := wrapReasoning("claude-opus-5", payload)
	require.NotEmpty(t, sealed)

	got, ok := unwrapReasoning(sealed, "claude-opus-5")
	require.True(t, ok)
	assert.JSONEq(t, string(payload), string(got))
}

func TestWrapReasoningIgnoresEmptyPayload(t *testing.T) {
	assert.Nil(t, wrapReasoning("claude-opus-5", nil))
	assert.Nil(t, wrapReasoning("claude-opus-5", json.RawMessage{}))
}

func TestUnwrapReasoningRejects(t *testing.T) {
	sealed := wrapReasoning("claude-opus-5", json.RawMessage(`[{"type":"thinking"}]`))

	tests := []struct {
		name  string
		raw   json.RawMessage
		model string
	}{
		{name: "nothing stored", raw: nil, model: "claude-opus-5"},
		{name: "another model", raw: sealed, model: "claude-sonnet-4-6"},
		{name: "malformed envelope", raw: json.RawMessage("not json"), model: "claude-opus-5"},
		{name: "empty payload", raw: json.RawMessage(`{"model":"claude-opus-5"}`), model: "claude-opus-5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := unwrapReasoning(tt.raw, tt.model)
			assert.False(t, ok)
		})
	}
}
