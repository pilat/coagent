package builtin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBrowserProfileSchemaBytes(t *testing.T) {
	t.Parallel()
	got := (browserProfile{}).Schema(
		json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}`),
	)
	const golden = `{"properties":{"task_state":{"description":"In up to five sentences, say whether the previous action succeeded, failed, or is uncertain based on the current frame; preserve task facts from this frame (names, prices, URLs, counts, exclusions); and name the next immediate goal. Do not describe screen coordinates or layout.","type":"string"},"x":{"type":"integer"}},"required":["x","task_state"],"type":"object"}`
	require.Equal(t, golden, string(got)) //nolint:testifylint // Schema bytes form the prompt-cache key.
}
