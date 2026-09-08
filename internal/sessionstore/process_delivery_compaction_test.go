package sessionstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/transcript"
)

func TestProcessDeliverySurvivesCompactionConcurrentInputAndRetry(t *testing.T) {
	store, _, projectID := newTestStore(t)
	ctx := context.Background()
	session, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	oldUser, err := store.InsertMessage(ctx, session.ID, &transcript.Message{
		Role: llmwire.RoleUser, Content: "old task",
	})
	require.NoError(t, err)
	oldAnswer, err := store.InsertMessage(ctx, session.ID, &transcript.Message{
		Role: llmwire.RoleAssistant, Content: "old answer",
	})
	require.NoError(t, err)

	assistant := &transcript.Message{
		Role: llmwire.RoleAssistant,
		ToolCalls: []byte(`[{"id":"process-event-1","name":"process_event","arguments":` +
			`{"process_id":"bgp_1","origin_session_id":1,"event":"completed"}}]`),
	}
	result := &transcript.Message{
		Role: llmwire.RoleTool, ToolCallID: "process-event-1", ToolName: "process_event",
		Content: "Process bgp_1 completed.",
	}
	start := make(chan struct{})
	errs := make(chan error, 3)
	insertedResult := make(chan bool, 1)

	go func() {
		<-start
		_, _, inserted, insertErr := store.InsertInternalToolNotificationPairOnce(
			ctx, session.ID, "bgp_1", "process-fingerprint", assistant, result,
		)
		insertedResult <- inserted
		errs <- insertErr
	}()
	go func() {
		<-start
		_, insertErr := store.InsertMessage(ctx, session.ID, &transcript.Message{
			Role: llmwire.RoleUser, Content: "concurrent user input",
		})
		errs <- insertErr
	}()
	go func() {
		<-start
		_, compactErr := store.ReplaceCompactedMessages(
			ctx,
			session.ID,
			[]int64{oldUser, oldAnswer},
			[]CompactionEntry{
				{Message: &transcript.Message{Role: llmwire.RoleUser, Content: "summary"}},
				{Message: &transcript.Message{Role: llmwire.RoleAssistant, Content: "ack"}},
			},
		)
		errs <- compactErr
	}()

	close(start)
	for range 3 {
		require.NoError(t, <-errs)
	}
	require.True(t, <-insertedResult)

	_, _, inserted, err := store.InsertInternalToolNotificationPairOnce(
		ctx, session.ID, "bgp_1", "process-fingerprint", assistant, result,
	)
	require.NoError(t, err)
	assert.False(t, inserted)

	messages, err := store.LoadActiveMessages(ctx, session.ID)
	require.NoError(t, err)
	processResults := 0
	foundConcurrentInput := false
	for _, message := range messages {
		if message.Role == llmwire.RoleTool && message.ToolName == "process_event" {
			processResults++
		}
		if message.Role == llmwire.RoleUser && message.Content == "concurrent user input" {
			foundConcurrentInput = true
		}
	}
	assert.Equal(t, 1, processResults)
	assert.True(t, foundConcurrentInput)
}
