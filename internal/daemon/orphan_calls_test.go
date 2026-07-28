package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func storedAssistant(toolCalls string) *sessionstore.StoredMessage {
	return &sessionstore.StoredMessage{Role: "assistant", ToolCalls: []byte(toolCalls)}
}

func storedToolResult(callID string) *sessionstore.StoredMessage {
	return &sessionstore.StoredMessage{Role: "tool", ToolCallID: callID, Content: "done"}
}

// The name-keyed pending set read from the durable transcript is what decides
// whether a call needs an owner at all, so its edges are load-bearing.
func TestUnresolvedStoredExternalCalls(t *testing.T) {
	tests := []struct {
		name string
		msgs []*sessionstore.StoredMessage
		want []session.PendingToolCall
	}{
		{
			name: "an unresolved external call is pending",
			msgs: []*sessionstore.StoredMessage{storedAssistant(`[{"id":"c1","name":"request_secret"}]`)},
			want: []session.PendingToolCall{{ID: "c1", Name: tool.IDRequestSecret}},
		},
		{
			name: "an answered call is not",
			msgs: []*sessionstore.StoredMessage{
				storedAssistant(`[{"id":"c1","name":"request_secret"}]`),
				storedToolResult("c1"),
			},
		},
		{
			name: "an in-loop tool is not external, however unresolved",
			msgs: []*sessionstore.StoredMessage{storedAssistant(`[{"id":"c1","name":"bash"}]`)},
		},
		{
			name: "a repeated call id is reported once",
			msgs: []*sessionstore.StoredMessage{
				storedAssistant(`[{"id":"c1","name":"sleep"}]`),
				storedAssistant(`[{"id":"c1","name":"sleep"}]`),
			},
			want: []session.PendingToolCall{{ID: "c1", Name: tool.IDSleep}},
		},
		{
			name: "a call with no id cannot be answered and is skipped",
			msgs: []*sessionstore.StoredMessage{storedAssistant(`[{"name":"sleep"}]`)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := unresolvedStoredExternalCalls(tt.msgs)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// A transcript row nobody can decode must fail the session's sweep, not be read
// as "nothing is pending here".
func TestUnresolvedStoredExternalCalls_UndecodableRow(t *testing.T) {
	_, err := unresolvedStoredExternalCalls([]*sessionstore.StoredMessage{storedAssistant(`{`)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode tool calls")
}

// Only a session that can still ship its transcript needs its calls closed;
// everything else is parked or gone.
func TestOrphanSweepCandidate(t *testing.T) {
	killedAt := time.Now()

	tests := []struct {
		name string
		rec  *sessionstore.SessionRecord
		want bool
	}{
		{name: "active", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusActive}, want: true},
		{name: "suspended", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusSuspended}, want: true},
		{name: "error", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusError}, want: true},
		{name: "completed", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusCompleted}},
		{name: "stopping", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusStopping}},
		{name: "stopped", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusStopped}},
		{name: "terminating", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusTerminating}},
		{
			name: "killed while suspended",
			rec:  &sessionstore.SessionRecord{Status: sessionstore.SessionStatusSuspended, KilledAt: &killedAt},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, orphanSweepCandidate(tt.rec))
		})
	}
}

// The notice tells the model what it can do next, and the terminal prompt is the
// one case where "ask again" is the whole answer.
func TestOrphanedCallNotice(t *testing.T) {
	assert.Contains(t, orphanedCallNotice(tool.IDRequestSecret), "Ask again")
	assert.Contains(t, orphanedCallNotice(tool.IDSetDefaultModel), "check the current state")
	assert.Contains(t, orphanedCallNotice(tool.IDSetDefaultModel), "restarted")
}
