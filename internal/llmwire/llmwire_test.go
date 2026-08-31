package llmwire

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ImageRef is persisted verbatim in messages.attachments, so its wire keys are
// frozen: dimensions must be additive and tolerant of rows written without them.
func TestImageRefDimensionsWireContract(t *testing.T) {
	var withDims ImageRef
	require.NoError(t, json.Unmarshal(
		[]byte(`{"path":"/a.png","mime":"image/png","size":10,"width":32,"height":20}`), &withDims))
	assert.Equal(t, 32, withDims.Width)
	assert.Equal(t, 20, withDims.Height)

	var legacy ImageRef
	require.NoError(t, json.Unmarshal(
		[]byte(`{"path":"/a.png","mime":"image/png","size":10}`), &legacy))
	assert.Zero(t, legacy.Width, "rows written before dimensions load without error")
	assert.Zero(t, legacy.Height)

	encoded, err := json.Marshal(legacy)
	require.NoError(t, err)
	assert.JSONEq(t, `{"path":"/a.png","mime":"image/png","size":10}`, string(encoded),
		"absent dimensions must not gain zero-valued keys")
}
