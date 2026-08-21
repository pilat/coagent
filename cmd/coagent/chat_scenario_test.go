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

func TestHarnessScenario_ModelCommandSwitchesTheCurrentSession(t *testing.T) {
	srv := newChatServer(t, socketPath(t), 42)
	srv.models = []controllerapi.ConfigModelInfo{
		{ID: "anthropic/claude", DisplayName: "Claude"},
		{
			ID: "openai/gpt-5", DisplayName: "GPT-5",
			EffortLevels: []string{"low", "high"}, DefaultEffort: "low",
		},
	}
	run := startChat(t, srv.socket, newScriptedTerminal(10*time.Millisecond))

	run.term.lines <- "/model"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "2) GPT-5")
	}, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- "2"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "Reasoning effort")
	}, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- "2"

	require.Eventually(t, func() bool { return len(srv.modelChanges()) == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, []cli.SetModelParams{{
		SessionID: 42, Model: "openai/gpt-5", ReasoningLevel: "high",
	}}, srv.modelChanges())
	assert.Empty(t, srv.sentText(), "a local command is never conversation text")
	assert.Contains(t, run.out.String(), "Model: GPT-5 · effort: high")

	assert.Equal(t, exitOK, run.finish(t))
}

func TestHarnessScenario_ModelCommandSelectsModelBeforeFirstMessage(t *testing.T) {
	srv := newChatServer(t, socketPath(t), 0)
	srv.models = []controllerapi.ConfigModelInfo{
		{ID: "anthropic/claude", DisplayName: "Claude"},
		{ID: "openai/gpt-5", DisplayName: "GPT-5", DefaultEffort: "high"},
	}
	run := startChat(t, srv.socket, newScriptedTerminal(10*time.Millisecond))

	run.term.lines <- "/model"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "2) GPT-5")
	}, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- "2"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "New conversation model: GPT-5")
	}, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- "hello"

	require.Eventually(t, func() bool { return len(srv.sentParams()) == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, "openai/gpt-5", srv.sentParams()[0].Model)
	assert.Equal(t, "hello", srv.sentParams()[0].Text)
	assert.Empty(t, srv.modelChanges(), "there is no session to mutate before the first message")

	assert.Equal(t, exitOK, run.finish(t))
}

func TestHarnessScenario_ModelCommandReconnectsBeforeApplyingChoice(t *testing.T) {
	socket := socketPath(t)
	first := newChatServer(t, socket, 42)
	first.models = []controllerapi.ConfigModelInfo{
		{ID: "anthropic/claude", DisplayName: "Claude"},
		{ID: "openai/gpt-5", DisplayName: "GPT-5"},
	}
	run := startChat(t, socket, newScriptedTerminal(10*time.Millisecond))

	run.term.lines <- "/model"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "2) GPT-5")
	}, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, first.server.Close())

	second := newChatServer(t, socket, 99)
	second.models = first.models
	require.Eventually(t, func() bool { return run.chat.currentSession() == 99 }, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- "2"

	require.Eventually(t, func() bool { return len(second.modelChanges()) == 1 }, 10*time.Second, 5*time.Millisecond)
	assert.Equal(t, int64(99), second.modelChanges()[0].SessionID)
	assert.Equal(t, "openai/gpt-5", second.modelChanges()[0].Model)
	assert.Contains(t, run.out.String(), "daemon restarting…")
	assert.Contains(t, run.out.String(), "reconnected.")

	assert.Equal(t, exitOK, run.finish(t))
}

func TestHarnessScenario_PendingModelSurvivesReconnectToANewSession(t *testing.T) {
	socket := socketPath(t)
	first := newChatServer(t, socket, 0)
	first.models = []controllerapi.ConfigModelInfo{
		{ID: "anthropic/claude", DisplayName: "Claude"},
		{ID: "openai/gpt-5", DisplayName: "GPT-5"},
	}
	run := startChat(t, socket, newScriptedTerminal(10*time.Millisecond))

	run.term.lines <- "/model"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "2) GPT-5")
	}, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- "2"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "New conversation model: GPT-5")
	}, 5*time.Second, 5*time.Millisecond)

	require.NoError(t, first.server.Close())

	second := newChatServer(t, socket, 77)
	require.Eventually(t, func() bool { return run.chat.currentSession() == 77 }, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- "hello"

	require.Eventually(t, func() bool { return len(second.sentParams()) == 1 }, 10*time.Second, 5*time.Millisecond)
	assert.Equal(t, int64(0), second.sentParams()[0].SessionID,
		"zero asks the manager to adopt its current session and apply the pending model")
	assert.Equal(t, "openai/gpt-5", second.sentParams()[0].Model)

	assert.Equal(t, exitOK, run.finish(t))
}

func TestHarnessScenario_ExplicitModelChoiceReplacesPendingAfterReconnect(t *testing.T) {
	socket := socketPath(t)
	first := newChatServer(t, socket, 0)
	first.models = []controllerapi.ConfigModelInfo{
		{ID: "anthropic/claude", DisplayName: "Claude"},
		{ID: "openai/gpt-5", DisplayName: "GPT-5"},
	}
	run := startChat(t, socket, newScriptedTerminal(10*time.Millisecond))

	run.term.lines <- "/model"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "2) GPT-5")
	}, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- "1"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "New conversation model: Claude")
	}, 5*time.Second, 5*time.Millisecond)

	require.NoError(t, first.server.Close())

	second := newChatServer(t, socket, 77)
	second.models = first.models
	require.Eventually(t, func() bool { return run.chat.currentSession() == 77 }, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- "/model"
	require.Eventually(t, func() bool {
		return strings.Count(run.out.String(), "2) GPT-5") >= 2
	}, 10*time.Second, 5*time.Millisecond)
	run.term.lines <- "2"
	require.Eventually(t, func() bool { return len(second.modelChanges()) == 1 }, 5*time.Second, 5*time.Millisecond)

	run.term.lines <- "hello"
	require.Eventually(t, func() bool { return len(second.sentParams()) == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, int64(77), second.sentParams()[0].SessionID)
	assert.Empty(t, second.sentParams()[0].Model, "the superseded pending model must not be applied")

	assert.Equal(t, exitOK, run.finish(t))
}

func TestHarnessScenario_ModelCommandShowsCurrentNonDefaultEffort(t *testing.T) {
	srv := newChatServer(t, socketPath(t), 42)
	srv.models = []controllerapi.ConfigModelInfo{{
		ID: "openai/gpt-5", DisplayName: "GPT-5",
		EffortLevels: []string{"low", "high"}, DefaultEffort: "low",
	}}
	srv.current = "openai/gpt-5"
	srv.effort = "high"
	run := startChat(t, srv.socket, newScriptedTerminal(10*time.Millisecond))

	run.term.lines <- "/model"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "Choose model")
	}, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- "1"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "high (current)")
	}, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- ""

	require.Eventually(t, func() bool { return len(srv.modelChanges()) == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, exitOK, run.finish(t))
}

func TestHarnessScenario_ModelCommandCancelKeepsChatOpen(t *testing.T) {
	srv := newChatServer(t, socketPath(t), 42)
	srv.models = []controllerapi.ConfigModelInfo{{ID: "anthropic/claude", DisplayName: "Claude"}}
	run := startChat(t, srv.socket, newScriptedTerminal(10*time.Millisecond))

	run.term.lines <- "/model"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "Choose model")
	}, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- ""
	run.term.lines <- "still chatting"

	require.Eventually(t, func() bool { return len(srv.sentText()) == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, []string{"still chatting"}, srv.sentText())
	assert.Empty(t, srv.modelChanges())

	assert.Equal(t, exitOK, run.finish(t))
}

func TestHarnessScenario_ModelCommandEnterUsesDefaultEffort(t *testing.T) {
	srv := newChatServer(t, socketPath(t), 42)
	srv.models = []controllerapi.ConfigModelInfo{{
		ID: "openai/gpt-5", DisplayName: "GPT-5",
		EffortLevels: []string{"low", "high"}, DefaultEffort: "high",
	}}
	run := startChat(t, srv.socket, newScriptedTerminal(10*time.Millisecond))

	run.term.lines <- "/model"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "Choose model")
	}, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- "1"
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "Choose effort")
	}, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- ""

	require.Eventually(t, func() bool { return len(srv.modelChanges()) == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, "high", srv.modelChanges()[0].ReasoningLevel)

	assert.Equal(t, exitOK, run.finish(t))
}

type chatRun struct {
	chat *chat
	term *scriptedTerminal
	out  *syncBuffer
	err  *syncBuffer
	code chan int
}

// A credential typed at the masked prompt reaches set_secret and nothing else. The
// request lands while a line read is outstanding — the sequence that used to leak.
func TestHarnessScenario_ChatSecretNeverReachesTheChatStream(t *testing.T) {
	srv := newChatServer(t, socketPath(t), 42)
	run := startChat(t, srv.socket, newScriptedTerminal(0))

	// The loop is parked in a line read: what the user types next is already
	// promised to somebody.
	requireEntered(t, run.term)

	srv.push(t, cli.SecretRequestMethod, cli.SecretRequest{
		SessionID: 42,
		RequestID: "req-1",
		Name:      "TELEGRAM_BOT_TOKEN",
		Purpose:   "the bot token",
	})

	require.Eventually(t, func() bool { return len(run.chat.secrets) == 1 }, 5*time.Second, 5*time.Millisecond)

	const value = "sk-live-typed-at-the-open-prompt"

	run.term.lines <- value

	require.Eventually(t, func() bool { return len(srv.storedSecrets()) == 1 }, 5*time.Second, 5*time.Millisecond)

	stored := srv.storedSecrets()[0]
	assert.Equal(t, "TELEGRAM_BOT_TOKEN", stored.Name)
	assert.Equal(t, value, stored.Value)
	assert.Equal(t, "req-1", stored.RequestID)

	assert.Empty(t, srv.sentText(), "the credential must never travel as chat text")
	assert.Contains(t, run.out.String(), "the bot token")
	assert.NotContains(t, run.out.String(), value)

	assert.Equal(t, exitOK, run.finish(t))
}

// With no line read outstanding the same request is collected the way it is
// meant to be: one masked read, still never a chat message.
func TestHarnessScenario_ChatSecretPromptReadsMasked(t *testing.T) {
	srv := newChatServer(t, socketPath(t), 7)
	run := startChat(t, srv.socket, newScriptedTerminal(10*time.Millisecond))

	srv.push(t, cli.SecretRequestMethod, cli.SecretRequest{SessionID: 7, RequestID: "req-2", Name: "OPENAI_API_KEY"})

	require.Eventually(t, func() bool { return run.term.masked.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Contains(t, run.out.String(), "OPENAI_API_KEY (hidden, empty line to decline): ")

	run.term.secrets <- "sk-masked"

	require.Eventually(t, func() bool { return len(srv.storedSecrets()) == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, "sk-masked", srv.storedSecrets()[0].Value)
	assert.Empty(t, srv.sentText())

	run.term.lines <- "/stop"

	require.Eventually(t, func() bool { return len(srv.stopped()) == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, []int64{7}, srv.stopped())

	assert.Equal(t, exitOK, run.finish(t))
}

// An empty line at the masked prompt declines the request, so the session stops
// waiting on a person who is not going to type it, and nothing is stored.
func TestHarnessScenario_ChatEmptyMaskedLineDeclinesTheRequest(t *testing.T) {
	srv := newChatServer(t, socketPath(t), 5)
	run := startChat(t, srv.socket, newScriptedTerminal(10*time.Millisecond))

	srv.push(t, cli.SecretRequestMethod, cli.SecretRequest{
		SessionID: 5, RequestID: "req-9", Name: "OPENAI_API_KEY", Purpose: "the provider key",
	})

	require.Eventually(t, func() bool { return run.term.masked.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Contains(t, run.out.String(), "empty line to decline", "the way out has to be on screen")

	run.term.secrets <- ""

	require.Eventually(t, func() bool { return len(srv.declinedSecrets()) == 1 }, 5*time.Second, 5*time.Millisecond)

	declined := srv.declinedSecrets()[0]
	assert.Equal(t, "req-9", declined.RequestID)
	assert.Equal(t, int64(5), declined.SessionID)

	assert.Empty(t, srv.storedSecrets(), "a declined prompt stores nothing")
	assert.Empty(t, srv.sentText(), "and says nothing in the conversation")

	// The chat is a normal chat again once the prompt is gone.
	run.term.lines <- "so what now?"
	require.Eventually(t, func() bool { return len(srv.sentText()) == 1 }, 5*time.Second, 5*time.Millisecond)

	assert.Equal(t, exitOK, run.finish(t))
}

// Two terminals can hold the same prompt open. When the other one answers, this
// one must stop asking — otherwise the next thing typed here dies inside a
// refused secret answer instead of reaching the conversation.
func TestHarnessScenario_ChatDismissedPromptStopsSwallowingInput(t *testing.T) {
	srv := newChatServer(t, socketPath(t), 21)
	run := startChat(t, srv.socket, newScriptedTerminal(10*time.Millisecond))

	srv.push(t, cli.SecretRequestMethod, cli.SecretRequest{
		SessionID: 21, RequestID: "req-1", Name: "MANAGER_TG_BOT_TOKEN", Purpose: "the bot token",
	})

	require.Eventually(t, func() bool { return run.term.masked.Load() == 1 }, 5*time.Second, 5*time.Millisecond)

	srv.push(t, cli.SecretResolvedMethod, cli.SecretResolved{
		SessionID: 21, RequestID: "req-1", Name: "MANAGER_TG_BOT_TOKEN",
	})

	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "MANAGER_TG_BOT_TOKEN: provided in another terminal")
	}, 5*time.Second, 5*time.Millisecond)

	// The chat is a chat again: the next line is a message, not a credential.
	run.term.lines <- "so what now?"

	require.Eventually(t, func() bool { return len(srv.sentText()) == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, []string{"so what now?"}, srv.sentText())
	assert.Empty(t, srv.storedSecrets(), "a dismissed prompt stores nothing")
	assert.Empty(t, srv.declinedSecrets(), "and answers nothing on the way out")
	assert.Equal(t, int32(1), run.term.masked.Load(), "the prompt closed instead of reopening")
	assert.Equal(t, 1, strings.Count(run.out.String(), "provided in another terminal"))

	assert.Equal(t, exitOK, run.finish(t))
}

// A request resolved before this terminal got round to showing it is dropped from
// the queue without a word: there was never a prompt on screen to explain.
func TestHarnessScenario_ChatDismissedQueuedPromptIsDropped(t *testing.T) {
	srv := newChatServer(t, socketPath(t), 22)
	run := startChat(t, srv.socket, newScriptedTerminal(0))

	requireEntered(t, run.term)

	srv.push(t, cli.SecretRequestMethod, cli.SecretRequest{
		SessionID: 22, RequestID: "req-2", Name: "OPENAI_API_KEY",
	})
	require.Eventually(t, func() bool { return len(run.chat.secrets) == 1 }, 5*time.Second, 5*time.Millisecond)

	srv.push(t, cli.SecretResolvedMethod, cli.SecretResolved{
		SessionID: 22, RequestID: "req-2", Name: "OPENAI_API_KEY",
	})
	require.Eventually(t, func() bool { return pendingDismissals(run.chat) == 1 }, 5*time.Second, 5*time.Millisecond)

	run.term.lines <- "unrelated question"

	require.Eventually(t, func() bool { return len(srv.sentText()) == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, []string{"unrelated question"}, srv.sentText())
	assert.Empty(t, srv.storedSecrets())
	assert.Zero(t, run.term.masked.Load(), "a prompt nobody saw is not opened just to be closed")
	assert.NotContains(t, run.out.String(), "provided in another terminal")

	assert.Equal(t, exitOK, run.finish(t))
}

// The dismissal goes to every terminal, the winner included. Its own answer coming
// back is an echo, not news, and printing it would be a lie.
func TestHarnessScenario_ChatOwnSecretAnswerIsNotAnnouncedBack(t *testing.T) {
	srv := newChatServer(t, socketPath(t), 23)
	run := startChat(t, srv.socket, newScriptedTerminal(10*time.Millisecond))

	srv.push(t, cli.SecretRequestMethod, cli.SecretRequest{
		SessionID: 23, RequestID: "req-3", Name: "OPENAI_API_KEY",
	})
	require.Eventually(t, func() bool { return run.term.masked.Load() == 1 }, 5*time.Second, 5*time.Millisecond)

	run.term.secrets <- "sk-mine"
	require.Eventually(t, func() bool { return len(srv.storedSecrets()) == 1 }, 5*time.Second, 5*time.Millisecond)

	srv.push(t, cli.SecretResolvedMethod, cli.SecretResolved{
		SessionID: 23, RequestID: "req-3", Name: "OPENAI_API_KEY",
	})

	// The push reader is ordered, so a later event on screen proves the dismissal
	// was already handled — which is what makes the silence below an assertion.
	srv.push(t, cli.EventMethod, cli.Event{
		SessionID: 23, Type: string(sessionevent.NotifyMessage), Message: "still here",
	})
	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "still here")
	}, 5*time.Second, 5*time.Millisecond)

	assert.NotContains(t, run.out.String(), "provided in another terminal")
	assert.Zero(t, pendingDismissals(run.chat), "its own answer is not a dismissal to act on")

	run.term.lines <- "next question"
	require.Eventually(t, func() bool { return len(srv.sentText()) == 1 }, 5*time.Second, 5*time.Millisecond)

	assert.Equal(t, exitOK, run.finish(t))
}

func startChat(t *testing.T, socket string, term *scriptedTerminal) *chatRun {
	t.Helper()

	return startChatWithin(t, socket, term, 5*time.Second)
}

// startChatWithin runs the chat with a reconnect budget set before the loop
// starts: the loop reads those fields, so a test may not write them afterwards.
func startChatWithin(t *testing.T, socket string, term *scriptedTerminal, budget time.Duration) *chatRun {
	t.Helper()

	run := &chatRun{term: term, out: &syncBuffer{}, err: &syncBuffer{}, code: make(chan int, 1)}
	run.chat = newChat(socket, term, run.out, run.err)
	run.chat.budget = budget
	run.chat.poll = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { run.code <- run.chat.run(ctx) }()

	require.Eventually(t, func() bool { return run.chat.currentClient() != nil }, 10*time.Second, 5*time.Millisecond)

	return run
}

// finish closes the input, which is a terminal's end-of-file, and reports the
// chat's exit code.
func (r *chatRun) finish(t *testing.T) int {
	t.Helper()

	select {
	case code := <-r.code:
		return code
	default:
	}

	close(r.term.lines)

	select {
	case code := <-r.code:
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("the chat did not exit")

		return exitError
	}
}

// pendingDismissals counts requests the chat was told about but has not yet let
// go of — the state a dismissal has to pass through to be observable at all.
func pendingDismissals(c *chat) int {
	c.secretMu.Lock()
	defer c.secretMu.Unlock()

	return len(c.dismissed)
}

func requireEntered(t *testing.T, term *scriptedTerminal) {
	t.Helper()

	select {
	case <-term.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the input loop never reached a line read")
	}
}
