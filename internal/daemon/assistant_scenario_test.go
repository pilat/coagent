package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func TestHarnessScenario_AssistantOpenAndRetell(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(system string, messages []llmwire.Message) *llmwire.Response {
		if !strings.HasPrefix(system, registry.AssistantPrompt) {
			return textReply("work answer")
		}

		_, event, _ := strings.Cut(messages[0].Content, "## event")
		if strings.Contains(event, "answered:") {
			return textReply(assistantJSON("retold answer", ""))
		}

		return textReply(assistantJSON("checking", `{"open":{"project":"deskwork","text":"check this"}}`))
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	again, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	assert.Equal(t, root.ID, again.ID)
	assert.Equal(t, tool.AssistantAgentType, root.AgentType)
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "please check"))
	h.waitUntil("assistant retelling", func() bool { return assistantSayCount(t, h, root.ID, "retold answer") == 1 })
	h.waitUntil("assistant idle", func() bool { return !h.mgr.HasActiveLoop(root.ID) })
	assert.Empty(t, h.messages(root.ID))
	assert.Equal(t, 1, assistantSayCount(t, h, root.ID, "checking"))
	var work int64
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT session_id FROM assistant_sends`).Scan(&work))
	assert.Equal(t, 1, assistantEventCount(t, h, root.ID))
	var workOutputs int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT count(*) FROM session_outbox
		WHERE session_id = ? AND type = 'message_persistent' AND content LIKE 'work answer%'`, work).Scan(&workOutputs))
	assert.Equal(t, 1, workOutputs)

	require.NoError(t, h.mgr.sendToSession(h.ctx, work, "direct user follow-up"))
	h.waitUntil("direct reply finished", func() bool { return !h.mgr.HasActiveLoop(work) })
	assert.Equal(t, 1, assistantEventCount(t, h, root.ID))
	h.mgr.progress.Reconcile(h.ctx, time.Now().UTC().Add(6*time.Minute))
	var cards int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT count(*) FROM session_outbox
		WHERE session_id = ? AND type = 'message_replaceable'`, root.ID).Scan(&cards))
	assert.Zero(t, cards)
}

func TestHarnessScenario_AssistantOpenRegisteredWorktree(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(system string, _ []llmwire.Message) *llmwire.Response {
		if strings.HasPrefix(system, registry.AssistantPrompt) {
			return textReply(assistantJSON("worktree result", ""))
		}

		return textReply("worktree answer")
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	path, name := t.TempDir(), "repo-abcd1234/feature"
	project, err := h.store.GetOrCreateNamedProject(h.ctx, path, name)
	require.NoError(t, err)
	a := &assistantSessions{daemon: h.mgr, root: root.ID, owner: scenarioManagerID}
	h.startInboxWake()
	card, err := a.Open(h.ctx, name, "inspect existing worktree")
	require.NoError(t, err)
	assert.Equal(t, name, card.Project)
	work, err := h.store.GetSession(h.ctx, card.ID)
	require.NoError(t, err)
	assert.Equal(t, project, work.ProjectID)
	h.waitUntil("worktree result", func() bool {
		return assistantSayCount(t, h, root.ID, "worktree result") == 1
	})

	for _, invalid := range []string{"unknown/feature", "../outside", "sys_coagent"} {
		_, err := a.Open(h.ctx, invalid, "must not create")
		require.Error(t, err)
	}
}

func TestHarnessScenario_AssistantSendResumesStoppedAndRefusesKill(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(system string, _ []llmwire.Message) *llmwire.Response {
		if strings.HasPrefix(system, registry.AssistantPrompt) {
			return textReply(assistantJSON("done", ""))
		}

		return textReply("resumed work")
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	require.NoError(t, h.store.UpdateSessionStatus(h.ctx, work.ID, sessionstore.SessionStatusStopped))
	h.startInboxWake()
	a := &assistantSessions{daemon: h.mgr, root: root.ID, owner: scenarioManagerID}
	require.NoError(t, a.Send(h.ctx, work.ID, "resume"))
	h.waitUntil("resumed assistant result", func() bool { return assistantSayCount(t, h, root.ID, "done") == 1 })
	h.waitUntil("assistant idle", func() bool { return !h.mgr.HasActiveLoop(root.ID) })

	for _, command := range []string{killCommand, clearCommand} {
		require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, command))
		var state string
		require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT state FROM session_inbox
			WHERE session_id = ? AND raw_content = ? ORDER BY id DESC LIMIT 1`, root.ID, command).Scan(&state))
		assert.Equal(t, "rejected", state)
		record, err := h.store.GetSession(h.ctx, root.ID)
		require.NoError(t, err)
		assert.Nil(t, record.KilledAt)
		assert.NotEqual(t, sessionstore.SessionStatusTerminating, record.Status)
	}

	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "still alive"))
	h.waitUntil("assistant still responds", func() bool { return assistantSayCount(t, h, root.ID, "done") == 2 })
}

func TestHarnessScenario_AssistantSupersededSend(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(system string, _ []llmwire.Message) *llmwire.Response {
		if strings.HasPrefix(system, registry.AssistantPrompt) {
			return textReply(assistantJSON("combined", ""))
		}

		return textReply("one work answer")
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)

	for _, text := range []string{"first instruction", "second instruction"} {
		require.NoError(t, h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
			in, err := sessionstore.EnqueueTx(h.ctx, tx, sessionstore.Input{
				SessionID: work.ID,
				Source:    sessionstore.InputSourceUser, Content: text, Attributes: map[string]any{"from": "assistant"},
			})
			if err != nil {
				return err
			}

			return assistant.RecordSendTx(h.ctx, tx, assistant.Send{
				InputID:            in.Input.ID,
				AssistantSessionID: root.ID, SessionID: work.ID, CreatedAt: time.Now().UTC(),
			})
		}))
	}

	h.startInboxWake()
	require.NoError(t, h.mgr.start(h.ctx, work.ID))
	h.waitUntil("combined result", func() bool { return assistantSayCount(t, h, root.ID, "combined") == 1 })
	h.waitUntil("assistant idle", func() bool { return !h.mgr.HasActiveLoop(root.ID) })
	assert.Equal(t, 1, assistantEventCount(t, h, root.ID))
	sends, err := h.mgr.assistantStore.UnobservedSends(h.ctx, work.ID)
	require.NoError(t, err)
	assert.Empty(t, sends)
	var accepted int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT count(*) FROM session_inbox
		WHERE session_id = ? AND source = 'user' AND state = 'accepted'`, work.ID).Scan(&accepted))
	assert.Equal(t, 2, accepted)
}

func assistantJSON(say, actions string) string {
	return fmt.Sprintf(`{"say":%q,"focus":"","stickers":[],"diary":{},"do":[%s]}`, say, actions)
}

func assistantSayCount(t *testing.T, h *harness, id int64, text string) int {
	t.Helper()
	var count int
	require.NoError(t, h.db.QueryRowContext(context.Background(), `SELECT count(*) FROM session_outbox
		WHERE session_id = ? AND type = 'message_persistent' AND content = ?`, id, text).Scan(&count))

	return count
}

func assistantEventCount(t *testing.T, h *harness, id int64) int {
	t.Helper()
	var count int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT count(*) FROM session_inbox
		WHERE session_id = ? AND source = 'agent'`, id).Scan(&count))

	return count
}
