package daemon

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
)

const helpWithGWT = "## Session commands\n" +
	"`/status` — show session status\n" +
	"`/stop` — stop the current run\n" +
	"`/clear` — start a fresh session\n" +
	"`/kill` — close this session\n" +
	"`/compact [focus]` — compact the context\n" +
	"`/schedules` — list schedules\n" +
	"`/budget <request>` — arm, replace, inspect, or clear a one-shot cost/wall-time checkpoint\n" +
	"`/gwt <name>` — fork into a worktree (Telegram session topics only)"

func TestHarnessScenario_HelpIncludesGWT(t *testing.T) {
	var modelCalls atomic.Int64
	h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)

		return &llmwire.Response{Text: "session ready"}
	})
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "open session", "fake-model", map[string]any{
		controllerapi.SessionAttributeManagerID: scenarioManagerID,
		"channel":                               "telegram",
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, sessionID, "session ready")

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "/help"))
	waitForVisibleMessage(t, collector, sessionID, helpWithGWT)

	controller := newChainController(t, h)
	drainScenarioClaims(t, "help_includes_gwt.json", controller)
	waitForIdleAfterMessage(t, collector, sessionID, helpWithGWT)

	assert.Equal(t, int64(1), modelCalls.Load(), "/help must not invoke the model")
	assertHarnessTrace(t, "help_includes_gwt.json", collector.snapshot(), sessionID)
}
