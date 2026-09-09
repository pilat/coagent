package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

func TestHasWaitingMarker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text string
		want bool
	}{
		{text: "<WAITING/>", want: true},
		{text: "reasoning\n <WAITING/> \nmore", want: true},
		{text: "wait <WAITING/> now", want: false},
		{text: "<WAITING>", want: false},
	}
	for _, tt := range tests {
		if got := hasWaitingMarker(tt.text); got != tt.want {
			t.Errorf("hasWaitingMarker(%q) = %t, want %t", tt.text, got, tt.want)
		}
	}
}

func TestWithoutWaitingMarker(t *testing.T) {
	t.Parallel()

	if got := withoutWaitingMarker("progress\n<WAITING/>\nmore"); got != "progress\nmore" {
		t.Fatalf("withoutWaitingMarker = %q", got)
	}
}

func TestRunLoop_WaitingMarkerPersistsTextAndSuppressesTools(t *testing.T) {
	t.Parallel()

	read := &countingTool{id: "read"}
	agent := newTestAgent(read)
	agent.hasLiveWakeSource = func(context.Context) bool { return true }
	agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{{
		Text:      "still working\n<WAITING/>\npark now",
		ToolCalls: []llmwire.ToolCall{{ID: "poll", Name: "read", Arguments: []byte(`{}`)}},
	}}}

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))
	require.NoError(t, err)
	assert.True(t, result.Suspended)
	assert.Zero(t, read.runs.Load())

	messages := agent.ms.getMessages()
	require.Len(t, messages, 1)
	assert.Equal(t, llmwire.RoleAssistant, messages[0].Role)
	assert.Equal(t, "still working\n<WAITING/>\npark now", messages[0].Content)
	assert.Empty(t, messages[0].ToolCalls)
}

func TestRunLoop_WaitingMarkerWithoutWakeSourceDoesNotSuspend(t *testing.T) {
	t.Parallel()

	read := &countingTool{id: "read"}
	agent := newTestAgent(read)
	agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{
		{
			Text:      "stray\n<WAITING/>",
			ToolCalls: []llmwire.ToolCall{{ID: "read-once", Name: "read", Arguments: []byte(`{}`)}},
		},
		{Text: "done"},
	}}

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))
	require.NoError(t, err)
	assert.False(t, result.Suspended)
	assert.Equal(t, int64(1), read.runs.Load())
}
