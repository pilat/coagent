package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// A manager-owned root whose model stops twice with no wake source: the first
// stop stays hidden as the durable candidate, only the confirmed second stop
// reaches the manager. The candidate text must never appear in any outbox row.
func TestHarnessScenario_CompletionCheckConfirmsBeforePublishing(t *testing.T) {
	var calls int
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		calls++
		if calls == 1 {
			return &llmwire.Response{Text: "premature candidate answer"}
		}

		return &llmwire.Response{Text: "confirmed final answer"}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	root, err := h.mgr.Send(h.ctx, h.projectID, "do the work", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, root, "confirmed final answer")

	drainScenarioClaims(t, "completion_check_confirmed_final.json", newChainController(t, h))
	waitForIdleAfterMessage(t, collector, root, "confirmed final answer")

	assert.Equal(t, 2, calls, "the no-wake stop costs exactly one confirmation call")

	var candidateLeaks int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND content LIKE '%premature candidate answer%'`, root).
		Scan(&candidateLeaks))
	assert.Zero(t, candidateLeaks, "the hidden candidate must never reach any outbox row")

	var persistent int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND type = 'message_persistent'`, root).Scan(&persistent))
	assert.Equal(t, 1, persistent, "exactly one persistent manager answer commits")

	var releases int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND type = 'message_persistent' AND releases_input = 1`, root).
		Scan(&releases))
	assert.Equal(t, 1, releases, "the confirmed output releases the manager input")

	assertHarnessTrace(t, "completion_check_confirmed_final.json", collector.snapshot(), root)
}

// A root that stops with non-empty text while its advertised background process
// still runs: the durable wake source owns the next turn, so the stop is
// trusted — one model call, no completion nudge, ordinary persistent output,
// and the runner released until the process completion delivers its input.
func TestHarnessScenario_CompletionCheckBackgroundProcessYieldPublishesOnce(t *testing.T) {
	var calls atomic.Int64
	processRunning := make(chan struct{})
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		n := calls.Add(1)
		if n == 1 {
			// The first stop is decided only after the test has advertised a
			// running process, so the wake projection sees the ledger row.
			<-processRunning

			return &llmwire.Response{Text: "yielding to the running process"}
		}

		if hasUserContaining(messages, "<process_completion>") {
			return &llmwire.Response{Text: "resumed after process completion"}
		}

		return &llmwire.Response{Text: "follow-up answer"}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	service := installScenarioProcessService(t, h)

	root, err := h.mgr.Send(h.ctx, h.projectID, "start a process", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)

	// The process holds until the test releases it, so the completion wake
	// lands after the yield has settled.
	release := filepath.Join(t.TempDir(), "release")
	process := startScenarioProcess(t, service, root, root,
		fmt.Sprintf("while [ ! -f %s ]; do sleep 0.05; done; printf 'done\\n'", release))
	waitScenarioProcessState(t, h, process.ID, backgroundprocess.StateRunning)
	close(processRunning)

	waitForVisibleMessage(t, collector, root, "yielding to the running process")
	h.mgr.waitIdle(root)

	assert.Equal(t, int64(1), calls.Load(), "a wake yield trusts the stop without a confirmation call")

	var yieldOutputs int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND type = 'message_persistent' AND content LIKE 'yielding to the running process%'`,
		root).Scan(&yieldOutputs))
	assert.Equal(t, 1, yieldOutputs, "the wake yield publishes ordinary output once")

	for _, message := range h.parentMessages(root) {
		if message.Role == llmwire.RoleUser {
			assert.NotContains(t, message.Content, "returned a final answer",
				"no completion nudge accompanies a wake yield")
		}
	}

	require.NoError(t, os.WriteFile(release, []byte("go"), 0o644))
	waitForVisibleMessage(t, collector, root, "resumed after process completion")
	assert.Equal(t, int64(3), calls.Load(),
		"the completion wake resumes through the ordinary two-phase check")
}

// An empty stop with the same durable wake source yields immediately too: no
// candidate, no nudge, no empty-streak movement, and one model call only.
func TestHarnessScenario_CompletionCheckEmptyBackgroundYieldYieldsSilently(t *testing.T) {
	var calls atomic.Int64
	processRunning := make(chan struct{})
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		n := calls.Add(1)
		if n == 1 {
			<-processRunning

			return &llmwire.Response{}
		}

		return &llmwire.Response{Text: "resumed after silent yield"}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	service := installScenarioProcessService(t, h)

	root, err := h.mgr.Send(h.ctx, h.projectID, "start a process and wait", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)

	release := filepath.Join(t.TempDir(), "release")
	process := startScenarioProcess(t, service, root, root,
		fmt.Sprintf("while [ ! -f %s ]; do sleep 0.05; done; printf 'done\\n'", release))
	waitScenarioProcessState(t, h, process.ID, backgroundprocess.StateRunning)
	close(processRunning)

	h.mgr.waitIdle(root)
	assert.Equal(t, int64(1), calls.Load(), "an empty wake yield ends the activation immediately")

	var nudges int
	for _, message := range h.parentMessages(root) {
		if message.Role == llmwire.RoleUser && strings.Contains(message.Content, "empty response") {
			nudges++
		}
	}
	assert.Zero(t, nudges, "an empty wake yield appends no empty-stop nudge")

	require.NoError(t, os.WriteFile(release, []byte("go"), 0o644))
	waitForVisibleMessage(t, collector, root, "resumed after silent yield")
}

// A stopped or killed undelivered child link promises no wake: the stop opens
// the two-phase check like any no-wake turn, so the first answer stays hidden
// and only the confirmed second stop reaches the manager.
func TestHarnessScenario_CompletionCheckStoppedLinkIsNotAWakeSource(t *testing.T) {
	for _, state := range []string{"stopped", "killed"} {
		t.Run(state, func(t *testing.T) {
			var calls atomic.Int64
			linkSeeded := make(chan struct{})
			respond := func(string, []llmwire.Message) *llmwire.Response {
				n := calls.Add(1)
				if n == 1 {
					// The dead link must exist before the first stop decision.
					<-linkSeeded

					return &llmwire.Response{Text: "premature answer over dead child"}
				}

				return &llmwire.Response{Text: "confirmed answer over dead child"}
			}

			h := newSubagentHarnessWith(t, respond)
			collector := collectEvents(h.mgr.PubSub().SubscribeAll())
			defer func() {
				collector.stop()
				h.shutdown()
			}()

			root, err := h.mgr.Send(h.ctx, h.projectID, "work while child is dead", "fake-model", map[string]any{
				"manager_id": scenarioManagerID,
			})
			require.NoError(t, err)

			child, err := h.sessStore.CreateSubagentSession(h.ctx, h.projectID, root, root, "general", "fake-model", "")
			require.NoError(t, err)
			_, err = h.db.ExecContext(h.ctx, `INSERT INTO subagent_links
				(parent_id, child_id, task_call_id, blocking, depth, state, created_at)
				VALUES (?, ?, ?, 0, 0, ?, ?)`,
				root, child, "task-dead", state, time.Now().UTC().Unix())
			require.NoError(t, err)
			close(linkSeeded)

			waitForVisibleMessage(t, collector, root, "confirmed answer over dead child")

			assert.Equal(t, int64(2), calls.Load(),
				"a %s link promises no wake: the two-phase check runs", state)
		})
	}
}

// A crash after the tool-bearing response commits clears confirmation state
// before execution; the restarted daemon settles the interrupted call
// per ADR-0059 and the next no-tool stop opens a fresh completion check.
func TestHarnessScenario_CompletionCheckCrashAfterToolPersistenceRestartsClean(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "completion-tool-crash.db")

	first := newSubagentHarnessOnDB(t, dbPath, func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, tool.IDSleep) {
			return &llmwire.Response{Text: "after sleep"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: "sleep-call", Name: tool.IDSleep,
			Arguments: []byte(`{"duration":"10m","reason":"hold the turn"}`),
		}}}
	}, nil)

	root, err := first.mgr.Send(first.ctx, first.projectID, "sleep then answer", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	firstCollector := collectEvents(first.mgr.PubSub().SubscribeAll())
	defer firstCollector.stop()
	waitForWaitKind(t, firstCollector, root, sessionevent.WaitSleep)

	// The tool-bearing response committed with its cleared completion state;
	// the daemon dies before the sleep resolves.
	state, stateErr := first.sessStore.LoadCompletionCheckState(first.ctx, root)
	require.NoError(t, stateErr)
	assert.Nil(t, state.CandidateID, "a tool-bearing response clears any pending check")
	first.shutdown()

	second := newSubagentHarnessOnDB(t, dbPath, func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "<interrupted>") {
			return &llmwire.Response{Text: "settled interrupted call"}
		}

		return &llmwire.Response{Text: "fresh confirmation after crash"}
	}, nil)
	defer second.shutdown()
	second.mgr.sweep(second.ctx)

	restarted, err := second.sessStore.LoadCompletionCheckState(second.ctx, root)
	require.NoError(t, err)
	assert.Nil(t, restarted.CandidateID, "no pending check survives beside a durable tool request")
}

// A budget crossing on the first hidden candidate suppresses both the nudge
// and the candidate text: only the host budget checkpoint publishes, and the
// model text appears in no outbox row.
func TestHarnessScenario_CompletionCheckBudgetCrossingOnCandidateHidesText(t *testing.T) {
	var calls atomic.Int64
	budgetArmed := make(chan struct{})
	respond := func(string, []llmwire.Message) *llmwire.Response {
		n := calls.Add(1)
		if n == 1 {
			// The budget row must exist before the first disposition commit;
			// admission has already passed with a zero tree cost.
			<-budgetArmed
		}

		// The cost lands with the candidate message, so the crossing is
		// observed by the disposition transaction, not by an earlier admission.
		return &llmwire.Response{Text: "unconfirmed candidate under budget", CostUSD: 0.01}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	root, err := h.mgr.Send(h.ctx, h.projectID, "do work under a tight budget", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)

	// The limit sits below the candidate's cost: admission sees a zero tree,
	// and only the disposition commit crosses it.
	_, err = h.db.ExecContext(h.ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, ?, 0, 0.000001)`, root, time.Now().UTC())
	require.NoError(t, err)
	close(budgetArmed)

	// The checkpoint publishes through the durable outbox; the park scheduler
	// owns its delivery, so assert the committed row rather than a push event.
	h.waitUntil("budget checkpoint committed", func() bool {
		var checkpoints int
		if loadErr := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
			WHERE session_id = ? AND content LIKE 'Budget checkpoint reached%'`, root).
			Scan(&checkpoints); loadErr != nil {
			return false
		}

		return checkpoints == 1
	})

	assert.Equal(t, int64(1), calls.Load(),
		"the crossing fires on the candidate disposition, before any confirmation call")

	var leaks int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND content LIKE '%unconfirmed candidate under budget%'`, root).
		Scan(&leaks))
	assert.Zero(t, leaks, "the unconfirmed candidate text never publishes")

	record, err := h.sessStore.GetBudget(h.ctx, root)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.BudgetFired, record.State)
}
