package tool

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
