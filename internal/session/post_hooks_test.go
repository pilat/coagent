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

type postFeedbackHooks struct {
	feedback []string
}

func (postFeedbackHooks) BeforeCall(context.Context, tool.Call) tool.HookVerdict {
	return tool.HookVerdict{}
}

func (h postFeedbackHooks) AfterCall(context.Context, tool.Call, *tool.Result) []string {
	return h.feedback
}

func TestPostToolUseFeedbackFollowsUntrustedWrapper(t *testing.T) {
	feedback := "PostToolUse:external hook feedback: [guard]: inspect the result"
	agent := newTestAgent(&imageStubTool{
		id: "external", result: &tool.Result{Output: "external evidence", Untrusted: true},
	})
	agent.callHooks = postFeedbackHooks{feedback: []string{feedback}}
	calls := []llmwire.ToolCall{{ID: "external-call", Name: "external", Arguments: []byte(`{}`)}}
	require.NoError(t, appendTestAssistant(t.Context(), agent.ms, &llmwire.Response{ToolCalls: calls}))
	require.NoError(t, executeToolCalls(t.Context(), agent, calls))
	rows := agent.store.(*mockSessionStore).messages
	require.Len(t, rows, 2)
	content := rows[1].Content
	markerID := requireUntrustedMarkerID(t, content)
	assert.True(t, strings.HasSuffix(content, `<<<END_UNTRUSTED_EXTERNAL_DATA id="`+markerID+`">>>`+"\n\n"+feedback))
	assert.Equal(t, 1, strings.Count(content, "<<<BEGIN_UNTRUSTED_EXTERNAL_DATA "))
	assert.Equal(t, 1, strings.Count(content, "<<<END_UNTRUSTED_EXTERNAL_DATA "))
}

func TestPostToolUseNoFeedbackPreservesToolRow(t *testing.T) {
	var contents []string
	for _, hooks := range []tool.CallHooks{nil, postFeedbackHooks{}} {
		agent := newTestAgent(&imageStubTool{id: "fixture", result: &tool.Result{Output: "unchanged result"}})
		agent.callHooks = hooks
		calls := []llmwire.ToolCall{{ID: "fixture-call", Name: "fixture", Arguments: []byte(`{}`)}}
		require.NoError(t, appendTestAssistant(t.Context(), agent.ms, &llmwire.Response{ToolCalls: calls}))
		require.NoError(t, executeToolCalls(t.Context(), agent, calls))
		rows := agent.store.(*mockSessionStore).messages
		require.Len(t, rows, 2)
		contents = append(contents, rows[1].Content)
	}
	assert.Equal(t, "unchanged result", contents[0])
	assert.Equal(t, contents[0], contents[1])
}
