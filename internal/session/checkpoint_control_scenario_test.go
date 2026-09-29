package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

type failOnceCheckpointBoundary struct {
	*loopInputBoundary
	store interface {
		HandleInput(context.Context, int64, string) error
	}
	handles int
}

func (b *failOnceCheckpointBoundary) Handle(ctx context.Context, input PendingInput, reason string) error {
	b.handles++
	if b.handles == 1 {
		return errors.New("terminal settlement unavailable")
	}
	return b.store.HandleInput(ctx, input.ID, reason)
}

// A request arriving during summarization belongs to the next safe point.
func TestCheckpointControl_RequestDuringSummarizationRunsLater(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, store, sessionID := newFinalOutputStore(t)
	firstPrompt := make(chan string, 1)
	releaseFirst := make(chan struct{})
	client := &compactionMockLLM{contextWindow: 200000}
	client.chat = func(call int, _ string) (*llmwire.Response, error) {
		instruction := client.lastMessages[len(client.lastMessages)-1].Content
		if call == 1 {
			firstPrompt <- instruction
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop}, nil
	}

	agent := newTestAgentWithStore(store, sessionID)
	agent.models = newTestModelRuntime(client, store, sessionID)
	agent.boundary = &loopInputBoundary{agent: agent}
	agent.outputStore = store
	agent.outputEnabled = true
	agent.turns = newToolTurns(agent.registry, agent.models, agent.ms, testProgressBoundary(agent.boundary))
	for _, message := range loopRounds(10, 4000) {
		require.NoError(t, agent.ms.appendMessageLocked(ctx, &message))
	}
	prepareDurableLoop(t, agent)

	input, err := store.EnqueueInput(ctx, sessionID, sessionstore.InputSourceUser, "/compact focus on the first bug")
	require.NoError(t, err)
	_, err = agent.contexts.queueCommand(ctx,
		PendingInput{ID: input.ID, Content: input.RawContent}, "focus on the first bug", nil)
	require.NoError(t, err)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		agent.contexts.apply(ctx, nil)
	}()

	select {
	case prompt := <-firstPrompt:
		assert.Contains(t, prompt, "Priority for this summary: focus on the first bug")
	case <-ctx.Done():
		t.Fatal("first summarization did not reach the model")
	}

	agent.RequestCompaction()
	close(releaseFirst)
	select {
	case <-firstDone:
	case <-ctx.Done():
		t.Fatal("first compaction did not finish")
	}

	firstSummary := agent.ms.getMessages()
	require.True(t, hasSummaryRow(firstSummary), "the first checkpoint committed")
	assert.True(t, agent.contexts.requested(), "the request made during I/O remains pending")
	active, err := store.LoadActiveMessages(ctx, sessionID)
	require.NoError(t, err)
	assert.Condition(t, func() bool {
		for _, message := range active {
			if isMarkedSummary(message.Content) {
				return true
			}
		}
		return false
	}, "the first checkpoint is durable")

	for _, message := range loopRounds(10, 4000) {
		// A new raw tail gives the later request a separate checkpoint to make.
		if message.Role == llmwire.RoleTool {
			message.Content = strings.ReplaceAll(message.Content, "x", "y")
		}
		require.NoError(t, agent.ms.appendMessageLocked(ctx, &message))
	}
	agent.contexts.apply(ctx, nil)
	assert.Equal(t, 2, client.callCount, "the later safe point runs the queued request")
	assert.NotContains(t, client.lastMessages[len(client.lastMessages)-1].Content,
		"focus on the first bug", "the first request's focus is one-shot")
	assert.False(t, agent.contexts.requested())
	active, err = store.LoadActiveMessages(ctx, sessionID)
	require.NoError(t, err)
	assert.NotEmpty(t, active)
	assert.True(t, hasSummaryRow(agent.ms.getMessages()))
}

// A committed summary must survive a retry of the separate command settlement.
func TestCheckpointControl_TerminalRetryDoesNotResummarize(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	db, store, sessionID := newFinalOutputStore(t)
	client := &compactionMockLLM{
		contextWindow: 200000,
		response: &llmwire.Response{
			Text: validSummary, FinishType: llmwire.FinishStop,
		},
	}
	agent := newTestAgentWithStore(store, sessionID)
	agent.models = newTestModelRuntime(client, store, sessionID)
	boundary := &failOnceCheckpointBoundary{
		loopInputBoundary: &loopInputBoundary{agent: agent},
		store:             store,
	}
	agent.boundary = boundary
	agent.turns = newToolTurns(agent.registry, agent.models, agent.ms, testProgressBoundary(agent.boundary))
	for _, message := range loopRounds(10, 4000) {
		require.NoError(t, agent.ms.appendMessageLocked(ctx, &message))
	}
	prepareDurableLoop(t, agent)

	input, err := store.EnqueueInput(ctx, sessionID, sessionstore.InputSourceUser, "/compact")
	require.NoError(t, err)
	_, err = agent.contexts.queueCommand(ctx, PendingInput{ID: input.ID, Content: input.RawContent}, "", nil)
	require.NoError(t, err)

	agent.contexts.apply(ctx, nil)
	require.Equal(t, 1, client.callCount)
	require.Equal(t, 1, boundary.handles)
	require.True(t, agent.contexts.requested(), "terminal settlement remains active")
	countSummaries := func() int {
		active, loadErr := store.LoadActiveMessages(ctx, sessionID)
		require.NoError(t, loadErr)
		count := 0
		for _, message := range active {
			if isMarkedSummary(message.Content) {
				count++
			}
		}
		return count
	}
	require.Equal(t, 1, countSummaries(), "the checkpoint committed before settlement failed")
	var state string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT state FROM session_inbox WHERE id = ?", input.ID).Scan(&state))
	require.Equal(t, "pending", state)

	agent.contexts.apply(ctx, nil)
	assert.Equal(t, 1, client.callCount, "terminal retry must not call the summarizer")
	assert.Equal(t, 2, boundary.handles)
	assert.False(t, agent.contexts.requested())
	assert.Equal(t, 1, countSummaries(), "terminal retry must not duplicate the summary")
	require.NoError(t, db.QueryRowContext(ctx, "SELECT state FROM session_inbox WHERE id = ?", input.ID).Scan(&state))
	assert.Equal(t, "handled", state)
}
