package sessioncalls

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

type scanTestAdapter struct {
	messages []llmwire.Message
	inserted []PendingToolCall
}

func (a *scanTestAdapter) Messages(context.Context) ([]llmwire.Message, error) {
	return a.messages, nil
}

func (a *scanTestAdapter) View() ([]llmwire.Message, error) { return a.messages, nil }

func (a *scanTestAdapter) InsertResult(_ context.Context, call PendingToolCall, _ string, _ bool) error {
	a.inserted = append(a.inserted, call)
	return nil
}

func (*scanTestAdapter) Reload(context.Context) error { return nil }

func TestScanRejectsDuplicateCallIDs(t *testing.T) {
	tests := []struct {
		name     string
		messages []llmwire.Message
	}{
		{
			name: "within assistant turn",
			messages: []llmwire.Message{{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{
				{ID: "same", Name: "read"}, {ID: "same", Name: "write"},
			}}},
		},
		{
			name: "across assistant turns while older call unresolved",
			messages: []llmwire.Message{
				{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "same", Name: "read"}}},
				{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "same", Name: "write"}}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot, err := Scan(tt.messages)
			require.ErrorIs(t, err, ErrAmbiguousCallID)
			assert.Empty(t, snapshot.Calls, "ambiguous transcripts must not expose a partial snapshot")
			assert.Empty(t, snapshot.GlobalUnresolved)
		})
	}
}

func TestScanPairsReusedCallIDInTranscriptOrder(t *testing.T) {
	messages := []llmwire.Message{
		{Role: llmwire.RoleTool, ToolCallID: "same"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "same", Name: "read"}}},
		{Role: llmwire.RoleTool, ToolCallID: "same"},
		{Role: llmwire.RoleTool, ToolCallID: "same"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "same", Name: "write"}}},
	}
	snapshot, err := Scan(messages)
	require.NoError(t, err)
	assert.Equal(t, []llmwire.ToolCall{{ID: "same", Name: "write"}}, snapshot.GlobalUnresolved)
	assert.Equal(t, map[string]string{"same": "write"}, snapshot.CurrentUnresolved)
	assert.Equal(t, CallStatus{Name: "write"}, snapshot.Calls["same"])

	messages = append(messages, llmwire.Message{Role: llmwire.RoleTool, ToolCallID: "same"})
	snapshot, err = Scan(messages)
	require.NoError(t, err)
	assert.Empty(t, snapshot.GlobalUnresolved)
	assert.Empty(t, snapshot.CurrentUnresolved)
	assert.Equal(t, CallStatus{Name: "write", Resolved: true}, snapshot.Calls["same"])
}

func TestSettleStoppedCallsDoesNotPartiallySettleAmbiguousTranscript(t *testing.T) {
	adapter := &scanTestAdapter{messages: []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "first", Name: "read"}}},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "first", Name: "write"}}},
	}}
	owner := NewOwner(TranscriptAdapter{
		Messages: adapter.Messages, View: adapter.View,
		InsertResult: adapter.InsertResult, Reload: adapter.Reload,
	}, nil, nil)
	err := owner.SettleStoppedCalls(context.Background(), "stopped")
	require.ErrorIs(t, err, ErrAmbiguousCallID)
	assert.Empty(t, adapter.inserted)
}
