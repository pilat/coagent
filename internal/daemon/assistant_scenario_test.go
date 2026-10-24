package daemon

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
)

var (
	assistantHandle   = regexp.MustCompile(`new-\d+-\d+`)
	assistantFocusRef = regexp.MustCompile(`\[f\d+\]`)
)

func TestHarnessScenario_AssistantOpenAndRetell(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(system string, messages []llmwire.Message) *llmwire.Response {
		if !strings.HasPrefix(system, registry.AssistantPrompt) {
			return textReply("work answer")
		}

		last := messages[len(messages)-1]
		_, event, _ := strings.Cut(messages[0].Content, "## event")

		switch {
		case len(messages) == 1 && strings.Contains(event, "answered:"):
			focus := assistantFocusRef.FindString(messages[0].Content)
			return assistantCalls("retold answer", llmwire.ToolCall{
				ID: "done", Name: "done_focus", Arguments: []byte(`{"id":"` + strings.Trim(focus, "[]") + `"}`),
			})
		case len(messages) == 1:
			return assistantCalls("", llmwire.ToolCall{
				ID:        "open",
				Name:      "open",
				Arguments: []byte(`{"project":"deskwork","about":"check","text":"check this"}`),
			})
		case last.ToolName == "open":
			handle := assistantHandle.FindString(last.Content)
			return assistantCalls("checking", llmwire.ToolCall{
				ID: "focus", Name: "focus", Arguments: []byte(`{"text":"Check","sessions":["` + handle + `"]}`),
			})
		default:
			return textReply("")
		}
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	again, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	assert.Equal(t, root.ID, again.ID)
	assert.Equal(t, sessionstore.AssistantAgentType, root.AgentType)
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
			return textReply("worktree result")
		}

		return textReply("worktree answer")
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	path, name := t.TempDir(), "repo-abcd1234/feature"
	project, err := h.store.GetOrCreateNamedProject(h.ctx, path, name)
	require.NoError(t, err)
	a := assistantFor(h, root.ID)
	h.startInboxWake()
	card, err := a.Open(h.ctx, name, "inspect existing worktree", "inspect existing worktree")
	require.NoError(t, err)
	assert.Equal(t, name, card.Project)
	work, err := h.store.GetSession(h.ctx, card.ID)
	require.NoError(t, err)
	assert.Equal(t, project, work.ProjectID)
	h.waitUntil("worktree result", func() bool {
		return assistantSayCount(t, h, root.ID, "worktree result") == 1
	})

	for _, invalid := range []string{"unknown/feature", "../outside", "sys_coagent"} {
		_, err := a.Open(h.ctx, invalid, "must not create", "invalid project")
		require.Error(t, err)
	}
}

func TestHarnessScenario_AssistantSendResumesStoppedAndRefusesKill(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(system string, _ []llmwire.Message) *llmwire.Response {
		if strings.HasPrefix(system, registry.AssistantPrompt) {
			return textReply("done")
		}

		return textReply("resumed work")
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	require.NoError(t, h.store.UpdateSessionStatus(h.ctx, work.ID, sessionstore.SessionStatusStopped))
	h.startInboxWake()
	a := assistantFor(h, root.ID)
	require.NoError(t, a.Send(h.ctx, work.ID, "resume"))
	h.waitUntil("resumed assistant result", func() bool { return assistantSayCount(t, h, root.ID, "done") == 1 })
	h.waitUntil("assistant idle", func() bool { return !h.mgr.HasActiveLoop(root.ID) })

	for _, command := range []string{killCommand, clearCommand} {
		require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, command))
		var state, reason string
		require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT state, resolution_reason FROM session_inbox
			WHERE session_id = ? AND raw_content = ? ORDER BY id DESC LIMIT 1`, root.ID, command).Scan(&state, &reason))
		assert.Equal(t, "handled", state)
		assert.Equal(t, "assistant_ignored_command", reason)
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
			return textReply("combined")
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

			_, err = tx.ExecContext(h.ctx, `INSERT INTO assistant_sends
				(input_id, assistant_session_id, session_id, created_at) VALUES (?, ?, ?, ?)`,
				in.Input.ID, root.ID, work.ID, time.Now().UTC())

			return err
		}))
	}

	h.startInboxWake()
	require.NoError(t, h.mgr.start(h.ctx, work.ID))
	h.waitUntil("combined result", func() bool { return assistantSayCount(t, h, root.ID, "combined") == 1 })
	h.waitUntil("assistant idle", func() bool { return !h.mgr.HasActiveLoop(root.ID) })
	assert.Equal(t, 1, assistantEventCount(t, h, root.ID))
	assert.Zero(t, unobservedCount(t, h, work.ID))
	var accepted int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT count(*) FROM session_inbox
		WHERE session_id = ? AND source = 'user' AND state = 'accepted'`, work.ID).Scan(&accepted))
	assert.Equal(t, 2, accepted)
}

type assistantDriver struct {
	h     *harness
	scope assistant.Scope
}

func assistantFor(h *harness, root int64) assistantDriver {
	return assistantDriver{h: h, scope: assistant.Scope{Root: root, Owner: scenarioManagerID}}
}

func (d assistantDriver) Open(ctx context.Context, project, text, about string) (assistant.Card, error) {
	return d.h.mgr.assistant.Open(ctx, assistant.Host(assistantHost{s: d.h.mgr}), d.scope, project, text, about)
}

func (d assistantDriver) Send(ctx context.Context, id int64, text string) error {
	return d.h.mgr.assistant.Send(ctx, assistant.Host(assistantHost{s: d.h.mgr}), d.scope, id, text)
}

func (d assistantDriver) Stop(ctx context.Context, id int64) error {
	return d.h.mgr.assistant.Stop(ctx, assistant.Host(assistantHost{s: d.h.mgr}), d.scope, id)
}

func (d assistantDriver) List(ctx context.Context, offset, limit int, project string) ([]assistant.Card, int, error) {
	return d.h.mgr.assistant.List(ctx, d.scope, offset, limit, project)
}

func (d assistantDriver) Recent(ctx context.Context, limit int) ([]assistant.Card, int, error) {
	return d.h.mgr.assistant.Recent(ctx, d.scope, limit)
}

func (d assistantDriver) Target(ctx context.Context, id int64) (assistant.Card, error) {
	return d.h.mgr.assistant.Target(ctx, d.scope, id)
}

func (d assistantDriver) Search(ctx context.Context, query string) ([]assistant.Hit, error) {
	return d.h.mgr.assistant.Search(ctx, d.scope, query)
}

// Read authorizes first, as the read tool does.
func (d assistantDriver) Read(ctx context.Context, id int64, count int) (string, error) {
	if _, err := d.Target(ctx, id); err != nil {
		return "", err
	}

	return d.h.mgr.assistant.Read(ctx, id, count)
}

func unobservedCount(t *testing.T, h *harness, id int64) int {
	t.Helper()
	var count int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT count(*) FROM assistant_sends
		WHERE observed_at IS NULL AND session_id = ?`, id).Scan(&count))

	return count
}

func assistantCalls(text string, calls ...llmwire.ToolCall) *llmwire.Response {
	return &llmwire.Response{Text: text, ToolCalls: calls}
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
