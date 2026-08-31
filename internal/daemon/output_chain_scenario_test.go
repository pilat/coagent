package daemon

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
)

// scenarioManagerID is the manager owner every output-chain scenario uses; the
// recorded claims must come from a real manager-bound controller.
const scenarioManagerID = "telegram:main"

func newChainController(t *testing.T, h *subagentHarness) controllerapi.OutputQueueController {
	t.Helper()

	queue, ok := newTestController(h.mgr, &config.Config{}, nil, nil).
		ForManager(scenarioManagerID).(controllerapi.OutputQueueController)
	require.True(t, ok)
	require.NoError(t, queue.BindOutputDelivery(t.Context(), controllerapi.OutputBindingData{
		Driver: "telegram",
		Attributes: map[string]any{
			"bot_user_id": int64(1), "chat_id": int64(2), "topology": "group",
		},
	}))

	return queue
}

// The reported order: one progress card, a pending follow-up that promotes only
// after tool settlement, a tool-only turn, a fresh-generation progress card,
// and the final answer. The claims prove generation-scoped replacement; the
// trace proves the ordered session events after each production ack.
func TestHarnessScenario_OutputChainReportedOrder(t *testing.T) {
	var calls int
	entered := make(chan struct{})
	followUpQueued := make(chan struct{})
	var once, closeOnce sync.Once
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		calls++
		switch calls {
		case 1:
			// Hold the first turn open until the follow-up is durably queued,
			// so promotion can only happen after the tool settlement.
			once.Do(func() { close(entered) })
			<-followUpQueued

			return &llmwire.Response{
				Text: "Reading the repo",
				ToolCalls: []llmwire.ToolCall{{
					ID: "chain-ls", Name: "ls", Arguments: []byte(`{"path":"."}`),
				}},
			}
		case 2:
			return &llmwire.Response{Text: "Stopping the mutation run", ToolCalls: []llmwire.ToolCall{
				{
					ID:   "chain-todo",
					Name: "todowrite",
					Arguments: []byte(
						`{"todos":[{"id":"t1","content":"ship the change","status":"in_progress","priority":"high"}]}`,
					),
				},
			}}
		default:
			return &llmwire.Response{Text: "All done."}
		}
	}

	h := newSubagentHarnessWith(t, respond)
	defer func() {
		closeOnce.Do(func() { close(followUpQueued) })
		h.shutdown()
	}()

	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer collector.stop()

	root, err := h.mgr.Send(h.ctx, h.projectID, "do the work", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "first model call")

	// The follow-up is enqueued while the first tool is unresolved, so it may
	// only enter history after settlement — advancing the generation exactly once.
	_, err = h.sessStore.EnqueueModelInput(h.ctx, root, "follow-up: also check the docs")
	require.NoError(t, err)
	closeOnce.Do(func() { close(followUpQueued) })

	waitForVisibleMessage(t, collector, root, "All done.")

	controller := newChainController(t, h)
	drainScenarioClaims(t, "output_chain_reported_order.json", controller)
	waitForIdleAfterMessage(t, collector, root, "All done.")

	assertHarnessTrace(t, "output_chain_reported_order.json", collector.snapshot(), root)
}
