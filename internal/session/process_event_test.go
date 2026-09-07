package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

func TestBuildProcessEventCompletionCarriesStableArguments(t *testing.T) {
	t.Parallel()

	pair, err := BuildProcessEventCompletion(ProcessEvent{
		ProcessID: "bgp_1", OriginSessionID: 42, State: "completed", OutputPath: "/tmp/output",
	})
	require.NoError(t, err)
	require.Len(t, pair, 2)

	var calls []llmwire.ToolCall
	require.NoError(t, json.Unmarshal(pair[0].ToolCalls, &calls))
	require.Len(t, calls, 1)
	assert.Equal(t, processEventTool, calls[0].Name)
	assert.JSONEq(t, `{"process_id":"bgp_1","origin_session_id":42,"event":"completed"}`,
		string(calls[0].Arguments))
	assert.Equal(t, calls[0].ID, pair[1].ToolCallID)
	assert.Equal(t, processEventTool, pair[1].ToolName)
}
