package daemon

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_AssistantForwardRefusesPreviousRunInput(t *testing.T) {
	var h *harness
	var root, work int64
	var previous *sessionstore.InboxInput
	var attempt sync.Once
	refused := make(chan error, 1)
	h = newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		if !hasUserContaining(messages, "event-only activation") {
			var err error
			previous, err = h.store.ActiveHumanInput(h.ctx, root)
			require.NoError(t, err)
			require.NotNil(t, previous)
			return textReply("human turn complete")
		}

		attempt.Do(func() {
			current, err := h.store.ActiveHumanInput(h.ctx, root)
			require.NoError(t, err)
			assert.Nil(t, current)
			err = (assistantHost{s: h.mgr}).Deliver(h.ctx, work, previous.RawContent,
				func(tx *sql.Tx, attributes map[string]any) (*sessionstore.InboxInput, error) {
					result, err := sessionstore.ForwardTx(h.ctx, tx, root, work, attributes)
					if err != nil {
						return nil, err
					}
					return result.Input, nil
				})
			refused <- err
		})
		return textReply("event turn complete")
	}})
	assistant, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	root = assistant.ID
	target, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	work = target.ID
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "the user's previous instruction"))
	h.waitUntil("human activation complete", func() bool {
		return assistantSayCount(t, h, root, "human turn complete") == 1 && !h.mgr.HasActiveLoop(root)
	})
	_, err = h.store.Enqueue(h.ctx, sessionstore.Input{
		SessionID: root, Source: sessionstore.InputSourceAgent, Content: "event-only activation",
	})
	require.NoError(t, err)
	h.waitUntil("event activation complete", func() bool {
		return assistantSayCount(t, h, root, "event turn complete") == 1 && !h.mgr.HasActiveLoop(root)
	})
	select {
	case err := <-refused:
		require.ErrorIs(t, err, sessionstore.ErrForwardInput)
	default:
		t.Fatal("event activation finished without attempting the forward")
	}
	var inputs, receipts int
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_inbox WHERE session_id = ?`, work).Scan(&inputs),
	)
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND source_key LIKE 'forward:%'`, root).Scan(&receipts))
	assert.Zero(t, inputs)
	assert.Zero(t, receipts)
	assert.False(t, h.mgr.HasActiveLoop(work))
	var userText string
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT raw_content FROM session_inbox WHERE id = ?`, previous.ID).Scan(&userText),
	)
	assert.Equal(t, "the user's previous instruction", userText)
}

func TestHarnessScenario_AssistantForwardToolRefusesPreviousRunInput(t *testing.T) {
	var work int64
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		if !hasUserContaining(messages, "event-only activation") {
			return textReply("human turn complete")
		}
		if hasToolResultFor(messages, "forward") {
			return textReply("event refusal complete")
		}
		return callReply("forward-old", "forward", fmt.Sprintf(`{"session":"%d"}`, work))
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	target, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	work = target.ID
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "the user's previous instruction"))
	h.waitUntil("human activation complete", func() bool {
		return assistantSayCount(t, h, root.ID, "human turn complete") == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	_, err = h.store.Enqueue(h.ctx, sessionstore.Input{
		SessionID: root.ID, Source: sessionstore.InputSourceAgent, Content: "event-only activation",
	})
	require.NoError(t, err)
	h.waitUntil("event refusal complete", func() bool {
		return assistantSayCount(t, h, root.ID, "event refusal complete") == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	assert.Contains(
		t,
		configLastToolResultContent(h.messages(root.ID), "forward"),
		sessionstore.ErrForwardInput.Error(),
	)
	var inputs, receipts int
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_inbox WHERE session_id = ?`, work).Scan(&inputs),
	)
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox WHERE session_id = ? AND source_key LIKE 'forward:%'`, root.ID).
			Scan(&receipts),
	)
	assert.Zero(t, inputs)
	assert.Zero(t, receipts)
}

func TestAssistantDuplicateForwardLeavesStoppedTarget(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	in, err := h.store.Enqueue(
		h.ctx,
		sessionstore.Input{SessionID: root.ID, Source: sessionstore.InputSourceUser, Content: "continue"},
	)
	require.NoError(t, err)
	_, err = h.store.Commit(h.ctx, sessionstore.Commit{
		SessionID: root.ID,
		BeginRun:  true,
		Accept: []sessionstore.Accept{
			{
				InputID:    in.Input.ID,
				State:      sessionstore.InputStateAccepted,
				Content:    in.Input.RawContent,
				LinkRef:    -1,
				ModelBound: true,
			},
		},
	})
	require.NoError(t, err)
	input, err := h.store.ActiveHumanInput(h.ctx, root.ID)
	require.NoError(t, err)
	require.NotNil(t, input)
	var forwarded *sessionstore.Enqueued
	require.NoError(t, h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
		var err error
		forwarded, err = sessionstore.ForwardTx(h.ctx, tx, root.ID, work.ID, nil)
		return err
	}))
	status := sessionstore.SessionStatusStopped
	_, err = h.store.Commit(h.ctx, sessionstore.Commit{
		SessionID: work.ID,
		Accept: []sessionstore.Accept{
			{
				InputID:    forwarded.Input.ID,
				State:      sessionstore.InputStateAccepted,
				Content:    forwarded.Input.RawContent,
				LinkRef:    -1,
				ModelBound: true,
			},
		},
		State: sessionstore.StatePatch{Status: &status},
	})
	require.NoError(t, err)
	h.store.TakeWoken()
	_, err = assistantFor(h, root.ID).execute(h.ctx, "forward", map[string]any{"session": work.ID})
	require.NoError(t, err)
	record, err := h.store.GetSession(h.ctx, work.ID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopped, record.Status)
	assert.False(t, h.mgr.HasActiveLoop(work.ID))
	assert.Empty(t, h.store.TakeWoken())
	var receipts int
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox WHERE session_id = ? AND source_key LIKE 'forward:%'`, root.ID).
			Scan(&receipts),
	)
	assert.Equal(t, 1, receipts)
}
