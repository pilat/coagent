package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func TestRunLoopReusedIDAfterCompactionGetsNewResult(t *testing.T) {
	db, store, sessionID := newFinalOutputStore(t)
	probe := &countingTool{id: "probe"}
	var agent *svc
	model := &loopScriptLLM{onCall: func(call int, _ []llmwire.Message) (*llmwire.Response, error) {
		switch call {
		case 1:
			return toolCallResponse("same-id", "probe"), nil
		case 2:
			messages, rowIDs := agent.ms.getMessages(), agent.ms.getRowIDs()
			var compacted []int64
			for i, message := range messages {
				if len(message.ToolCalls) > 0 || message.Role == llmwire.RoleTool {
					compacted = append(compacted, rowIDs[i])
				}
			}
			_, err := store.ReplaceCompactedMessages(t.Context(), sessionID, compacted,
				[]sessionstore.CompactionEntry{{Message: &transcript.Message{Role: llmwire.RoleUser, Content: "[summary]"}}})
			require.NoError(t, err)
			require.NoError(t, agent.ms.reloadMessages(t.Context()))
			return toolCallResponse("same-id", "probe"), nil
		default:
			return textResponse("done"), nil
		}
	}}
	agent = newTestAgentWithStore(store, sessionID, probe)
	setIntegrityModel(agent, model)
	require.NoError(t, agent.ms.addUserMessage(t.Context(), "do the work"))

	result, err := runTestLoop(t.Context(), t, agent, loopOptions{}, iterationGuard(8))
	require.NoError(t, err)
	assert.Equal(t, "done", result.FinalResponse)
	assert.Equal(t, int64(2), probe.runs.Load())
	var resultRows int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND role = 'tool' AND tool_call_id = 'same-id'`, sessionID).Scan(&resultRows))
	assert.Equal(t, 2, resultRows, "each invocation retains its own durable result")
}

func TestRunLoopAmbiguousResponsePersistsExactAssistantCallsWithoutExecution(t *testing.T) {
	for _, budgetCrossing := range []bool{false, true} {
		name := "ordinary"
		if budgetCrossing {
			name = "budget crossing"
		}
		t.Run(name, func(t *testing.T) {
			db, store, sessionID := newFinalOutputStore(t)
			probe := &countingTool{id: "probe"}
			calls := []llmwire.ToolCall{
				{ID: "reused", Name: "probe", Arguments: []byte(`{"first":true}`)},
				{ID: "reused", Name: "probe", Arguments: []byte(`{"second":true}`)},
			}
			response := &llmwire.Response{ToolCalls: calls, FinishType: llmwire.FinishToolCalls, CostUSD: 0.01}
			model := &loopScriptLLM{responses: []*llmwire.Response{response, textResponse("done")}}
			agent := newTestAgentWithStore(store, sessionID, probe)
			setIntegrityModel(agent, model)
			if budgetCrossing {
				armCheckpointBudget(t, store, sessionID)
				response.CostUSD = 1
			}
			require.NoError(t, agent.ms.addUserMessage(t.Context(), "do the work"))

			result, err := runTestLoop(t.Context(), t, agent, loopOptions{}, iterationGuard(4))
			if budgetCrossing {
				require.NoError(t, err)
				assert.True(t, result.Suspended)
				assert.Equal(t, 1, model.calls)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "done", result.FinalResponse)
				assert.Equal(t, 3, model.calls)
			}
			assert.Zero(t, probe.runs.Load(), "ambiguous calls must never reach tool execution")

			messages, err := store.LoadActiveMessages(t.Context(), sessionID)
			require.NoError(t, err)
			for _, message := range messages {
				assert.NotEqual(t, llmwire.RoleTool, message.Role, "the unusable calls receive no fabricated results")
				assert.Empty(t, message.ToolCalls, "unusable calls must leave active context")
			}
			var rawCalls string
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT tool_calls FROM messages
				WHERE session_id = ? AND role = 'assistant' AND compacted_at IS NOT NULL ORDER BY id LIMIT 1`,
				sessionID).Scan(&rawCalls))
			var persisted []llmwire.ToolCall
			require.NoError(t, json.Unmarshal([]byte(rawCalls), &persisted))
			assert.Equal(t, calls, persisted, "the paid attempt remains available for inspection")
		})
	}
}

func TestRunAmbiguousBudgetCrossingKeepsSingleCheckpointOutcome(t *testing.T) {
	db, store, sessionID := newFinalOutputStore(t)
	armCheckpointBudget(t, store, sessionID)
	probe := &countingTool{id: "probe"}
	agent := newTestAgentWithStore(store, sessionID, probe)
	agent.outputStore = store
	agent.outputEnabled = true
	setIntegrityModel(agent, &loopScriptLLM{responses: []*llmwire.Response{{
		FinishType: llmwire.FinishToolCalls, CostUSD: 1,
		ToolCalls: []llmwire.ToolCall{
			{ID: "reused", Name: "probe"}, {ID: "reused", Name: "probe"},
		},
	}}})
	prepareDurableLoop(t, agent)
	require.NoError(t, agent.ms.addUserMessage(t.Context(), "do the work"))
	priorOutbox := outboxRows(t, db, sessionID)

	result, err := agent.run(t.Context(), "")
	require.NoError(t, err)
	assert.True(t, result.Suspended)
	assert.Empty(t, result.ErrorNotice)
	assert.Zero(t, probe.runs.Load())
	rows := outboxRows(t, db, sessionID)
	require.Len(t, rows, len(priorOutbox)+1)
	assert.Equal(t, "budget:1:checkpoint", rows[len(rows)-1]["source_key"])
	record, err := store.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusSuspended, record.Status)
}
