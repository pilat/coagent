package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

// Trips on the delivery's tree-lock lookup, after the turn commits and before the lock and instruction.
type assistantCommitBarrier struct {
	Store
	db      *sql.DB
	target  int64
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func TestHarnessScenario_AssistantBackgroundResult(t *testing.T) {
	processRunning := make(chan struct{})
	t.Cleanup(func() { closeOnce(processRunning) })
	h := newHarness(t, harnessOptions{respond: func(system string, messages []llmwire.Message) *llmwire.Response {
		if strings.HasPrefix(system, registry.AssistantPrompt) {
			return textReply("full result")
		}

		if hasUserContaining(messages, "<process_completion>") {
			return textReply("answer after process")
		}

		<-processRunning

		return textReply("yield before process")
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	service := installScenarioProcessService(t, h)
	a := assistantFor(h, root.ID)
	require.NoError(t, a.Send(h.ctx, work.ID, "work with process"))
	release := filepath.Join(t.TempDir(), "release")
	startScenarioProcess(
		t,
		service,
		work.ID,
		work.ID,
		fmt.Sprintf("while [ ! -f '%s' ]; do sleep 0.05; done; echo done", release),
	)
	close(processRunning)
	h.waitUntil("work yielded", func() bool { return !h.mgr.HasActiveLoop(work.ID) })
	assert.Zero(t, assistantEventCount(t, h, root.ID))
	assert.Equal(t, 1, unobservedCount(t, h, work.ID))
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	h.waitUntil("process result retold", func() bool { return assistantSayCount(t, h, root.ID, "full result") == 1 })
	var body string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT raw_content FROM session_inbox
		WHERE session_id = ? AND source = 'agent'`, root.ID).Scan(&body))
	assert.Contains(t, body, `yield before process\n\n---\n\nanswer after process`,
		"every final is shown, oldest first")
	assert.Equal(t, 1, strings.Count(body, "answer after process"), "a repeated final is shown once")
}

func TestHarnessScenario_AssistantBootObservesCompletedSend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	first := newHarness(t, harnessOptions{dbPath: path})
	root, err := first.mgr.EnsureAssistantRoot(first.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := first.store.CreateSession(
		first.ctx,
		first.projectID,
		"fake-model",
		"",
		managerAttrs(scenarioManagerID),
	)
	require.NoError(t, err)
	var input int64
	require.NoError(t, first.store.WithTx(first.ctx, func(tx *sql.Tx) error {
		in, err := sessionstore.EnqueueTx(first.ctx, tx, sessionstore.Input{
			SessionID: work.ID, Source: sessionstore.InputSourceUser, Content: "finish before crash",
		})
		if err != nil {
			return err
		}

		input = in.Input.ID

		_, err = tx.ExecContext(first.ctx, `INSERT INTO assistant_sends
			(input_id, assistant_session_id, session_id, created_at) VALUES (?, ?, ?, ?)`,
			input, root.ID, work.ID, time.Now().UTC())

		return err
	}))
	completed := sessionstore.SessionStatusCompleted
	_, err = first.store.Commit(first.ctx, sessionstore.Commit{
		SessionID: work.ID,
		Accept: []sessionstore.Accept{
			{InputID: input, State: sessionstore.InputStateAccepted, Content: "finish before crash", LinkRef: -1},
		},
		Messages: []*transcript.Message{{Role: "assistant", Content: "finished before crash", FinishType: "stop"}},
		State:    sessionstore.StatePatch{Status: &completed},
	})
	require.NoError(t, err)
	first.shutdown()
	second := newHarness(
		t,
		harnessOptions{dbPath: path, respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
			return textReply("recovered answer")
		}},
	)
	require.NoError(t, second.mgr.Start(second.ctx))
	second.waitUntil(
		"recovered observation",
		func() bool { return assistantSayCount(t, second, root.ID, "recovered answer") == 1 },
	)
	require.NoError(t, second.mgr.sweepAssistantObservations(second.ctx))
	assert.Equal(t, 1, assistantEventCount(t, second, root.ID))
}

func TestHarnessScenario_AssistantShutdownAfterCommitKeepsEffects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gap.db")
	var workID int64
	first := newHarness(
		t,
		harnessOptions{dbPath: path, respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
			if len(messages) > 1 {
				return textReply("")
			}

			return assistantCalls("committed reply",
				llmwire.ToolCall{
					ID: "send", Name: "send",
					Arguments: fmt.Appendf(nil, `{"session":%d,"text":"not applied"}`, workID),
				},
				llmwire.ToolCall{
					ID: "focus", Name: "focus",
					Arguments: fmt.Appendf(nil, `{"text":"Await","sessions":["%d"]}`, workID),
				})
		}},
	)
	root, err := first.mgr.EnsureAssistantRoot(first.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := first.store.CreateSession(
		first.ctx,
		first.projectID,
		"fake-model",
		"",
		managerAttrs(scenarioManagerID),
	)
	require.NoError(t, err)
	workID = work.ID
	prior, err := first.store.Enqueue(first.ctx, sessionstore.Input{
		SessionID: workID, Source: sessionstore.InputSourceUser, Content: "prior assistant work",
	})
	require.NoError(t, err)
	_, err = first.store.Commit(first.ctx, sessionstore.Commit{
		SessionID: workID,
		Accept: []sessionstore.Accept{
			{
				InputID: prior.Input.ID,
				State:   sessionstore.InputStateAccepted,
				Content: "prior assistant work",
				LinkRef: -1,
			},
		},
		Messages: []*transcript.Message{
			{Role: "user", Content: "prior assistant work"},
			{Role: "assistant", Content: "prior result", FinishType: "stop"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, first.store.WithTx(first.ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(first.ctx, `INSERT INTO assistant_sends
			(input_id, assistant_session_id, session_id, created_at) VALUES (?, ?, ?, ?)`,
			prior.Input.ID, root.ID, workID, time.Now().UTC()); err != nil {
			return err
		}
		_, err := tx.ExecContext(first.ctx, `UPDATE assistant_sends SET observed_at = ?
			WHERE input_id = ? AND observed_at IS NULL`, time.Now().UTC(), prior.Input.ID)
		return err
	}))
	require.NoError(t, first.store.UpdateSessionStatus(first.ctx, workID, sessionstore.SessionStatusCompleted))
	barrier := &assistantCommitBarrier{
		Store: first.store, db: first.db, target: workID,
		reached: make(chan struct{}), release: make(chan struct{}),
	}
	first.mgr.store = barrier
	t.Cleanup(func() { closeOnce(barrier.release) })
	require.NoError(t, first.mgr.sendToSession(first.ctx, root.ID, "send work"))
	requireSignal(t, barrier.reached)
	first.mgr.life.close()
	close(barrier.release)
	first.shutdown()
	second := newHarness(t, harnessOptions{dbPath: path})
	require.NoError(t, second.mgr.Start(second.ctx))
	var state string
	require.NoError(
		t,
		second.db.QueryRowContext(second.ctx, `SELECT state FROM session_inbox WHERE session_id = ?`, root.ID).
			Scan(&state),
	)
	assert.Equal(t, "handled", state)
	var sent int
	require.NoError(t, second.db.QueryRowContext(second.ctx, `SELECT count(*) FROM session_inbox
		WHERE session_id = ? AND raw_content = 'not applied'`, workID).Scan(&sent))
	assert.Equal(t, 1, sent, "a graceful shutdown does not cut the committed turn's effects")
	var failures int
	require.NoError(t, second.db.QueryRowContext(second.ctx, `SELECT count(*) FROM session_inbox
		WHERE session_id = ? AND json_extract(attributes, '$.assistant_event') = 'action_failed'`, root.ID).
		Scan(&failures))
	assert.Zero(t, failures, "a durable send is not reported as failed")
	lines, err := second.mgr.assistant.RecentTape(second.ctx, root.ID, 40)
	require.NoError(t, err)
	require.Len(t, lines, 3)
	assert.Contains(t, lines[2].Text, "not applied")
	assert.Equal(t, 1, assistantSayCount(t, second, root.ID, "committed reply"))
}

func (b *assistantCommitBarrier) GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	if id != b.target {
		return b.Store.GetSession(ctx, id)
	}

	var committedStep bool
	if err := b.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM assistant_turns WHERE outcome = 'ok')`).
		Scan(&committedStep); err != nil {
		return nil, err
	}

	if committedStep {
		b.once.Do(func() { close(b.reached); <-b.release })
	}

	return b.Store.GetSession(ctx, id)
}

func TestHarnessScenario_AssistantWaitsForWorkSleep(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	var input int64
	require.NoError(t, h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
		in, err := sessionstore.EnqueueTx(h.ctx, tx, sessionstore.Input{
			SessionID: work.ID, Source: sessionstore.InputSourceUser, Content: "merge when green",
		})
		if err != nil {
			return err
		}

		input = in.Input.ID

		_, err = tx.ExecContext(h.ctx, `INSERT INTO assistant_sends
			(input_id, assistant_session_id, session_id, created_at) VALUES (?, ?, ?, ?)`,
			input, root.ID, work.ID, time.Now().UTC())

		return err
	}))
	_, err = h.store.Commit(h.ctx, sessionstore.Commit{
		SessionID: work.ID,
		Accept: []sessionstore.Accept{
			{InputID: input, State: sessionstore.InputStateAccepted, Content: "merge when green", LinkRef: -1},
		},
		Messages: []*transcript.Message{
			{Role: "assistant", Content: "merging, waiting for CI", FinishType: "stop"},
			{Role: "assistant", ToolCalls: json.RawMessage(`[{"ID":"call_sleep","Name":"sleep","Arguments":{}}]`)},
		},
	})
	require.NoError(t, err)

	require.NoError(t, h.mgr.assistant.Observe(h.ctx, work.ID, nil))
	assert.Zero(t, assistantEventCount(t, h, root.ID), "a session paused inside a tool call has not answered")

	_, err = h.store.Commit(h.ctx, sessionstore.Commit{
		SessionID: work.ID,
		Messages:  []*transcript.Message{{Role: "assistant", Content: "both merged", FinishType: "stop"}},
	})
	require.NoError(t, err)
	require.NoError(t, h.mgr.assistant.Observe(h.ctx, work.ID, nil))
	var body string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT raw_content FROM session_inbox
		WHERE session_id = ? AND source = 'agent'`, root.ID).Scan(&body))
	assert.Contains(t, body, "merging, waiting for CI")
	assert.Contains(t, body, "both merged")
}
