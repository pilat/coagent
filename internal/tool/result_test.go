package tool

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResultJSON(t *testing.T) {
	encoded, err := json.Marshal(Result{Output: "answered"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"output":"answered"}`, string(encoded))
}
