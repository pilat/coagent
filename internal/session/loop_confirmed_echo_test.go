package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

// The echo-keeps-FinalResponse decision belongs to one stop only: a confirmed
// check echoes the candidate (its ack is discarded), while any later final
// produced by the same loop notifies its own fresh text.
func TestHandlePreviousResultConfirmedFlagSemantics(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	var notified []string
	runner.opts = loopOptions{
		Notify: func(_ context.Context, msg string) error {
			notified = append(notified, msg)

			return nil
		},
		Working: func(bool) {},
	}

	runner.lastResp = textResponse("first answer")
	require.NoError(t, runner.recordIteration(ctx))
	runner.lastResp = textResponse("why I am stopping")
	require.NoError(t, runner.recordIteration(ctx))

	assert.True(t, runner.confirmedFinal, "a confirmed check sets the flag")
	assert.Equal(t, "first answer", runner.result.FinalResponse)

	// A later turn ends in a fresh final: runLoop resets the flag at the
	// iteration top before handlePreviousResult runs.
	runner.confirmedFinal = false
	runner.lastResp = textResponse("second turn answer")
	runner.agent.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: "next"},
		{Role: llmwire.RoleAssistant, Content: "second turn answer"},
	})

	done, err := runner.handlePreviousResult(ctx)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, []string{"second turn answer"}, notified,
		"a fresh final after a confirmed check notifies its own text")

	// With the flag still set (the confirming-stop case) the echo keeps the
	// disposition-settled candidate and the ack text is discarded.
	runner.confirmedFinal = true
	runner.result.FinalResponse = "first answer"
	notified = nil
	runner.lastResp = textResponse("third turn answer")
	runner.agent.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: "more"},
		{Role: llmwire.RoleAssistant, Content: "third turn answer"},
	})

	done, err = runner.handlePreviousResult(ctx)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, []string{"first answer"}, notified,
		"confirmedFinal keeps the candidate; the stop's own text is not echoed")
	_ = db
	_ = store
	_ = sessionID
}
