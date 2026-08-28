package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/managers/cli"
)

// TestHarnessScenario_ChatAttachAdoptsSnapshotWatermark pins the reconnect
// contract: chat_open renders the canonical snapshot, the terminal adopts its
// outbox watermark, and an idle push for an output the terminal has not shown
// can never release the prompt ahead of that output's delivery.
func TestHarnessScenario_ChatAttachAdoptsSnapshotWatermark(t *testing.T) {
	srv := newChatServer(t, socketPath(t), 42)
	srv.progress = "snapshot: iteration 7 of the long turn"
	srv.progressWatermark = 5
	run := startChat(t, srv.socket, newScriptedTerminal(0))

	require.Eventually(t, func() bool {
		return strings.Contains(run.out.String(), "snapshot: iteration 7 of the long turn")
	}, 5*time.Second, 5*time.Millisecond, "attach must render the captured snapshot")

	run.chat.mu.Lock()
	adopted := run.chat.outputID
	run.chat.mu.Unlock()
	assert.Equal(t, int64(5), adopted, "the terminal adopts the snapshot watermark")

	// Simulate a turn in flight: only the causal output may release the prompt.
	run.chat.setBusy(true)

	srv.push(t, cli.EventMethod, cli.Event{
		SessionID: 42, Type: "state_changed", Status: "idle", AfterOutputID: 7,
	})
	time.Sleep(100 * time.Millisecond)
	assert.True(t, run.chat.isBusy(),
		"an idle push for an undelivered output must not release the prompt")

	srv.push(t, cli.EventMethod, cli.Event{
		SessionID: 42, Type: "state_changed", Status: "idle", AfterOutputID: 5,
	})
	assert.Eventually(t, func() bool { return !run.chat.isBusy() },
		5*time.Second, 5*time.Millisecond,
		"the output the snapshot already covers releases the prompt")

	require.NoError(t, run.chat.reconnect(context.Background(), run.chat.currentClient()))
	run.chat.mu.Lock()
	afterReconnect := run.chat.outputID
	run.chat.mu.Unlock()
	assert.Equal(t, int64(5), afterReconnect, "reconnect keeps the adopted watermark")
	assert.Contains(t, run.out.String(), "snapshot: iteration 7 of the long turn")
}
