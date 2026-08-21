package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managers/cli"
	"github.com/pilat/coagent/internal/sessionevent"
)

func TestHarnessScenario_ChatReconnectKeepsOneEventReader(t *testing.T) {
	socket := socketPath(t)
	first := newChatServer(t, socket, 1)
	run := startChat(t, socket, newScriptedTerminal(10*time.Millisecond))

	run.term.lines <- "before the restart"
	require.Eventually(t, func() bool { return len(first.sentText()) == 1 }, 5*time.Second, 5*time.Millisecond)

	failed := run.chat.currentClient()
	require.NoError(t, first.server.Close())
	second := newChatServer(t, socket, 9)
	require.Eventually(t, func() bool {
		return run.chat.currentClient() != failed
	}, 5*time.Second, 5*time.Millisecond, "the replacement client must finish chat_open")
	assert.Equal(t, int64(9), run.chat.currentSession())

	run.term.lines <- "after the restart"
	require.Eventually(t, func() bool { return len(second.sentText()) == 1 }, 10*time.Second, 5*time.Millisecond)

	assert.Equal(t, []string{"after the restart"}, second.sentText())
	assert.Equal(t, int64(9), run.chat.currentSession(), "the chat re-attached to the resumed session")
	assert.Equal(t, int32(1), run.chat.pushLive.Load())
	assert.Equal(t, int32(1), run.chat.pushPeak.Load(), "a second push reader was left running")

	second.push(t, cli.EventMethod, cli.Event{SessionID: 9, Type: string(sessionevent.NotifyMessage), Message: "back"})
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "back")
	}, 5*time.Second, 5*time.Millisecond)

	assert.Contains(t, run.out.String(), "daemon restarting…")
	assert.Contains(t, run.out.String(), "reconnected.")

	require.NoError(t, run.chat.reconnect(context.Background(), run.chat.currentClient()))
	assert.Equal(t, int32(1), run.chat.pushLive.Load())
	assert.Equal(t, int32(1), run.chat.pushPeak.Load())

	assert.Equal(t, exitOK, run.finish(t))
}

// A config tool accepts the turn before restarting, so the chat must reconnect
// without new input to receive the answer produced by the new image.
func TestHarnessScenario_ChatReconnectsBeforeRestartedTurnReplies(t *testing.T) {
	socket := socketPath(t)
	first := newChatServer(t, socket, 1)
	run := startChat(t, socket, newScriptedTerminal(10*time.Millisecond))

	run.term.lines <- "add the model"
	require.Eventually(t, func() bool { return len(first.sentText()) == 1 }, 5*time.Second, 5*time.Millisecond)
	promptsBeforeRestart := strings.Count(run.out.String(), "> ")

	failed := run.chat.currentClient()
	require.NoError(t, first.server.Close())
	second := newChatServer(t, socket, 1)
	require.Eventually(t, func() bool {
		return run.chat.currentClient() != failed && len(second.attached()) == 1
	}, 5*time.Second, 5*time.Millisecond,
		"the dropped push stream must reconnect without another user message")

	second.push(t, cli.EventMethod, cli.Event{
		SessionID: 1,
		Type:      string(sessionevent.NotifyMessage),
		Message:   "model added",
	})
	second.push(t, cli.EventMethod, cli.Event{
		SessionID: 1,
		Type:      string(sessionevent.NotifyStateChanged),
		Status:    string(controllerapi.StateIdle),
	})
	require.Eventually(t, func() bool {
		return strings.Count(run.out.String(), "> ") == promptsBeforeRestart+1
	}, 5*time.Second, 5*time.Millisecond)

	out := run.out.String()
	restarting := strings.Index(out, "daemon restarting…")
	reconnected := strings.Index(out, "reconnected.")
	answer := strings.Index(out, "model added")
	require.NotEqual(t, -1, restarting)
	require.NotEqual(t, -1, reconnected)
	require.NotEqual(t, -1, answer)
	assert.Less(t, restarting, reconnected)
	assert.Less(t, reconnected, answer)
	assert.Equal(t, 1, strings.Count(out, "model added"))
	assert.Equal(t, exitOK, run.finish(t))
}

// A reconnect that runs out of budget ends the chat with the reason and clears
// the turn whose answer can no longer arrive.
func TestHarnessScenario_ChatFailedAutomaticReconnectExits(t *testing.T) {
	socket := socketPath(t)
	srv := newChatServer(t, socket, 3)
	run := startChatWithin(t, socket, newScriptedTerminal(10*time.Millisecond), 150*time.Millisecond)

	run.term.lines <- "apply the config"
	require.Eventually(t, func() bool { return len(srv.sentText()) == 1 }, 5*time.Second, 5*time.Millisecond)
	require.True(t, run.chat.isBusy())

	require.NoError(t, srv.server.Close())

	select {
	case code := <-run.code:
		assert.Equal(t, exitError, code)
	case <-time.After(5 * time.Second):
		t.Fatal("the chat did not report the failed reconnect")
	}

	assert.False(t, run.chat.isBusy(), "the unreachable turn must not keep the prompt suppressed")
	assert.Contains(t, run.err.String(), "did not come back")
	assert.Contains(t, run.out.String(), "daemon restarting…")
}

func TestHarnessScenario_ChatConcurrentReconnectFailureUsesOneBudget(t *testing.T) {
	socket := socketPath(t)
	srv := newChatServer(t, socket, 3)
	run := startChatWithin(t, socket, newScriptedTerminal(10*time.Millisecond), 150*time.Millisecond)
	failed := run.chat.currentClient()

	require.NoError(t, srv.server.Close())

	errCh := make(chan error, 1)
	go func() { errCh <- run.chat.reconnect(context.Background(), failed) }()

	select {
	case err := <-errCh:
		require.ErrorContains(t, err, "did not come back")
	case <-time.After(5 * time.Second):
		t.Fatal("the concurrent reconnect did not share the failed attempt")
	}

	select {
	case code := <-run.code:
		assert.Equal(t, exitError, code)
	case <-time.After(5 * time.Second):
		t.Fatal("the chat did not report the failed reconnect")
	}

	assert.Equal(t, 1, strings.Count(run.out.String(), "daemon restarting…"))
}

func TestHarnessScenario_ChatFailedAutomaticReconnectExitsModelPicker(t *testing.T) {
	socket := socketPath(t)
	srv := newChatServer(t, socket, 3)
	srv.models = []controllerapi.ConfigModelInfo{{ID: "anthropic/claude", DisplayName: "Claude"}}
	run := startChatWithin(t, socket, newScriptedTerminal(10*time.Millisecond), 150*time.Millisecond)

	run.term.lines <- "/model"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "Choose model")
	}, 5*time.Second, 5*time.Millisecond)

	require.NoError(t, srv.server.Close())

	select {
	case code := <-run.code:
		assert.Equal(t, exitError, code)
	case <-time.After(5 * time.Second):
		t.Fatal("the model picker did not report the failed reconnect")
	}

	assert.Contains(t, run.err.String(), "did not come back")
}

func TestHarnessScenario_ChatFailedAutomaticReconnectEndsMaskedPrompt(t *testing.T) {
	socket := socketPath(t)
	srv := newChatServer(t, socket, 3)
	run := startChatWithin(t, socket, newScriptedTerminal(10*time.Millisecond), 150*time.Millisecond)

	srv.push(t, cli.SecretRequestMethod, cli.SecretRequest{
		SessionID: 3, RequestID: "req-restart", Name: "OPENAI_API_KEY",
	})
	require.Eventually(t, func() bool { return run.term.inMask.Load() }, 5*time.Second, 5*time.Millisecond)

	require.NoError(t, srv.server.Close())

	select {
	case code := <-run.code:
		assert.Equal(t, exitError, code)
	case <-time.After(5 * time.Second):
		t.Fatal("the masked prompt did not report the failed reconnect")
	}

	assert.False(t, run.term.inMask.Load(), "the terminal must leave masked mode on reconnect failure")
	assert.Contains(t, run.err.String(), "did not come back")
}
