package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/managers/cli"
)

func TestHarnessScenario_ChatReconnectDoesNotDuplicateActiveSecret(t *testing.T) {
	socket := socketPath(t)
	first := newChatServer(t, socket, 3)
	run := startChat(t, socket, newScriptedTerminal(10*time.Millisecond))
	req := cli.SecretRequest{SessionID: 3, RequestID: "req-replay", Name: "OPENAI_API_KEY"}

	first.push(t, cli.SecretRequestMethod, req)
	require.Eventually(t, func() bool { return run.term.inMask.Load() }, 5*time.Second, 5*time.Millisecond)

	failed := run.chat.currentClient()
	require.NoError(t, first.server.Close())
	second := newReplayChatServer(t, socket, 3, []cli.SecretRequest{req}, false)
	require.Eventually(t, func() bool {
		return run.chat.currentClient() != failed && len(second.attached()) == 1
	}, 5*time.Second, 5*time.Millisecond, "the replacement client must finish chat_open")
	require.Eventually(t, func() bool {
		run.chat.secretMu.Lock()
		defer run.chat.secretMu.Unlock()

		_, ok := run.chat.replayed[req.RequestID]

		return ok
	}, 5*time.Second, 5*time.Millisecond, "the duplicate replay must be suppressed before the answer")

	run.term.secrets <- "sk-after-restart"
	require.Eventually(t, func() bool { return len(second.storedSecrets()) == 1 }, 5*time.Second, 5*time.Millisecond)

	run.term.lines <- "continue"
	require.Eventually(t, func() bool { return len(second.sentText()) == 1 }, 5*time.Second, 5*time.Millisecond)

	assert.Equal(t, int32(1), run.term.masked.Load(), "the replay must not open the same prompt twice")
	assert.Equal(t, exitOK, run.finish(t))
}

func TestHarnessScenario_ChatRequeuesSuppressedSecretReplayAfterAnswerFailure(t *testing.T) {
	socket := socketPath(t)
	first := newChatServer(t, socket, 3)
	run := startChat(t, socket, newScriptedTerminal(10*time.Millisecond))
	req := cli.SecretRequest{SessionID: 3, RequestID: "req-retry", Name: "OPENAI_API_KEY"}

	first.push(t, cli.SecretRequestMethod, req)
	require.Eventually(t, func() bool { return run.term.inMask.Load() }, 5*time.Second, 5*time.Millisecond)

	failed := run.chat.currentClient()
	require.NoError(t, first.server.Close())
	second := newReplayChatServer(t, socket, 3, []cli.SecretRequest{req}, true)
	require.Eventually(t, func() bool {
		return run.chat.currentClient() != failed && len(second.attached()) == 1
	}, 5*time.Second, 5*time.Millisecond, "the replacement client must finish chat_open")
	require.Eventually(t, func() bool {
		run.chat.secretMu.Lock()
		defer run.chat.secretMu.Unlock()

		_, ok := run.chat.replayed[req.RequestID]

		return ok
	}, 5*time.Second, 5*time.Millisecond, "the reconnect replay must arrive before the answer fails")

	run.term.secrets <- "first-attempt"
	require.Eventually(t, func() bool { return run.term.masked.Load() == 2 }, 5*time.Second, 5*time.Millisecond,
		"the replay suppressed before the failed answer must be requeued")

	run.term.secrets <- "second-attempt"
	require.Eventually(t, func() bool { return len(second.storedSecrets()) == 1 }, 5*time.Second, 5*time.Millisecond)
	run.term.lines <- "continue"
	require.Eventually(t, func() bool { return len(second.sentText()) == 1 }, 5*time.Second, 5*time.Millisecond)

	assert.Equal(t, exitOK, run.finish(t))
}

func newReplayChatServer(
	t *testing.T,
	socket string,
	sessionID int64,
	replay []cli.SecretRequest,
	failFirstSecret bool,
) *chatServer {
	t.Helper()

	srv, err := ctl.NewServer(context.Background(), socket, "test", ctl.Deps{Config: &config.Config{}})
	require.NoError(t, err)

	s := &chatServer{socket: socket, server: srv, sessionID: sessionID}
	var failSecret atomic.Bool
	failSecret.Store(failFirstSecret)
	open := func(ctx context.Context, c *ctl.Conn, params json.RawMessage) (any, *ctl.Error) {
		res, callErr := s.openOp(ctx, c, params)
		if callErr != nil {
			return nil, callErr
		}

		for _, req := range replay {
			if err := c.Notify(cli.SecretRequestMethod, req); err != nil {
				return nil, &ctl.Error{Code: ctl.CodeInternal, Message: err.Error()}
			}
		}

		return res, nil
	}

	require.NoError(t, srv.Register(cli.OpChatOpen, open))
	require.NoError(t, srv.Register(cli.OpChatSend, s.sendOp))
	require.NoError(t, srv.Register(cli.OpChatStop, s.stopOp))
	require.NoError(t, srv.Register(cli.OpChatModels, s.modelsOp))
	require.NoError(t, srv.Register(cli.OpChatSetModel, s.setModelOp))
	require.NoError(t, srv.Register(ctl.OpSetSecret, func(
		ctx context.Context,
		c *ctl.Conn,
		params json.RawMessage,
	) (any, *ctl.Error) {
		if failSecret.CompareAndSwap(true, false) {
			return nil, &ctl.Error{Code: ctl.CodeInternal, Message: "injected secret failure"}
		}

		return s.secretOp(ctx, c, params)
	}))
	require.NoError(t, srv.Register(cli.OpChatSecretCancel, s.cancelOp))

	go func() { _ = srv.Serve(context.Background()) }()
	t.Cleanup(func() { _ = srv.Close() })
	waitServing(t, socket)

	return s
}
