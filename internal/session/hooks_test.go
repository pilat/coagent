package session

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

type refusingCallHooks struct {
	reason string
}

func (h refusingCallHooks) BeforeCall(context.Context, tool.Call) tool.HookVerdict {
	return tool.HookVerdict{Refusal: h.reason}
}

func (refusingCallHooks) AfterCall(context.Context, tool.Call, *tool.Result) []string {
	return nil
}

func TestExecuteToolCalls_RefusedCallPersistsRefusalAsToolError(t *testing.T) {
	stub := newGateTool("guarded", false)
	agent := newTestAgent(stub)
	agent.callHooks = refusingCallHooks{reason: "operator refused this call"}
	calls := []llmwire.ToolCall{gateCall(stub.ID(), "guarded-call")}
	require.NoError(t, appendTestAssistant(t.Context(), agent.ms, &llmwire.Response{ToolCalls: calls}))
	require.NoError(t, executeToolCalls(t.Context(), agent, calls))
	rows := agent.store.(*mockSessionStore).messages
	require.Len(t, rows, 2)
	assert.True(t, rows[1].ToolError)
	assert.Equal(t, "operator refused this call", rows[1].Content)
	assert.NotContains(t, rows[1].Content, "UNTRUSTED_EXTERNAL_DATA")
	assert.Zero(t, stub.entryCount())
}

func TestRecordToolResults_HookFeedbackOutsideStampedUntrustedFrame(t *testing.T) {
	feedback := "operator context remains outside evidence"
	stub := &imageStubTool{
		id: "external",
		result: &tool.Result{
			Output: strings.Repeat("large external payload ", 20000), Untrusted: true,
			HookFeedback: []string{feedback},
		},
	}
	agent := newTestAgent(stub)
	calls := []llmwire.ToolCall{{ID: "external-call", Name: stub.ID(), Arguments: []byte(`{}`)}}
	require.NoError(t, appendTestAssistant(t.Context(), agent.ms, &llmwire.Response{ToolCalls: calls}))
	require.NoError(t, executeToolCalls(t.Context(), agent, calls))
	rows := agent.store.(*mockSessionStore).messages
	require.Len(t, rows, 2)
	content := rows[1].Content
	markerID := requireUntrustedMarkerID(t, content)
	endMarker := `<<<END_UNTRUSTED_EXTERNAL_DATA id="` + markerID + `">>>`
	assert.True(t, strings.HasSuffix(content, endMarker+"\n\n"+feedback))
	assert.Contains(t, content, "(omitted ")
	assert.Equal(t, 1, strings.Count(content, "<<<BEGIN_UNTRUSTED_EXTERNAL_DATA "))
	assert.Equal(t, 1, strings.Count(content, "<<<END_UNTRUSTED_EXTERNAL_DATA "))
}
