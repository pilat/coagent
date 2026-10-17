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

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

type assistantCommitBarrier struct {
	Store
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func TestHarnessScenario_AssistantBackgroundResult(t *testing.T) {
	processRunning := make(chan struct{})
	t.Cleanup(func() { closeOnce(processRunning) })
	h := newHarness(t, harnessOptions{respond: func(system string, messages []llmwire.Message) *llmwire.Response {
		if strings.HasPrefix(system, registry.AssistantPrompt) {
			return textReply(assistantJSON("full result", ""))
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
	a := &assistantSessions{daemon: h.mgr, root: root.ID, owner: scenarioManagerID}
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
	sends, err := h.mgr.assistantStore.UnobservedSends(h.ctx, work.ID)
	require.NoError(t, err)
	assert.Len(t, sends, 1)
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

		return assistant.RecordSendTx(first.ctx, tx, assistant.Send{
			InputID: input, SessionID: work.ID,
			AssistantSessionID: root.ID, CreatedAt: time.Now().UTC(),
		})
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
			return textReply(assistantJSON("recovered answer", ""))
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

func TestHarnessScenario_AssistantCrashAfterStepCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gap.db")
	var workID int64
	first := newHarness(t, harnessOptions{dbPath: path, respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
		return textReply(
			assistantJSON("committed reply", fmt.Sprintf(`{"send":{"session":%d,"text":"not applied"}}`, workID)),
		)
	}})
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
		if err := assistant.RecordSendTx(
			first.ctx,
			tx,
			assistant.Send{
				InputID:            prior.Input.ID,
				AssistantSessionID: root.ID,
				SessionID:          workID,
				CreatedAt:          time.Now().UTC(),
			},
		); err != nil {
			return err
		}
		_, err := assistant.MarkObservedTx(first.ctx, tx, prior.Input.ID, time.Now().UTC())
		return err
	}))
	require.NoError(t, first.store.UpdateSessionStatus(first.ctx, workID, sessionstore.SessionStatusCompleted))
	barrier := &assistantCommitBarrier{Store: first.store, reached: make(chan struct{}), release: make(chan struct{})}
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
	inputs, err := second.store.ListPending(second.ctx, workID)
	require.NoError(t, err)
	assert.Empty(t, inputs)
	var workInputs int
	require.NoError(
		t,
		second.db.QueryRowContext(second.ctx, `SELECT count(*) FROM session_inbox WHERE session_id = ?`, workID).
			Scan(&workInputs),
	)
	assert.Equal(t, 1, workInputs)
	lines, err := second.mgr.assistantStore.VisibleTape(second.ctx, root.ID, 40, 0)
	require.NoError(t, err)
	require.Len(t, lines, 3)
	assert.Contains(t, lines[2].Text, "not applied")
	assert.Equal(t, 1, assistantSayCount(t, second, root.ID, "committed reply"))
}

func (b *assistantCommitBarrier) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	committedStep := false
	err := b.Store.WithTx(ctx, func(tx *sql.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}

		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM assistant_turns WHERE outcome = 'ok')`).
			Scan(&committedStep)
	})
	if err == nil && committedStep {
		b.once.Do(func() { close(b.reached); <-b.release })
	}

	return err
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

		return assistant.RecordSendTx(h.ctx, tx, assistant.Send{
			InputID: input, SessionID: work.ID, AssistantSessionID: root.ID, CreatedAt: time.Now().UTC(),
		})
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

	require.NoError(t, h.mgr.observeAssistantWork(h.ctx, work.ID, nil))
	assert.Zero(t, assistantEventCount(t, h, root.ID), "a session paused inside a tool call has not answered")

	_, err = h.store.Commit(h.ctx, sessionstore.Commit{
		SessionID: work.ID,
		Messages:  []*transcript.Message{{Role: "assistant", Content: "both merged", FinishType: "stop"}},
	})
	require.NoError(t, err)
	require.NoError(t, h.mgr.observeAssistantWork(h.ctx, work.ID, nil))
	var body string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT raw_content FROM session_inbox
		WHERE session_id = ? AND source = 'agent'`, root.ID).Scan(&body))
	assert.Contains(t, body, "merging, waiting for CI")
	assert.Contains(t, body, "both merged")
}
