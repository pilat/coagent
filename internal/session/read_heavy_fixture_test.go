package session

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// readCalls builds n native read calls in one assistant turn.
func readCalls(n int) []llmwire.ToolCall {
	calls := make([]llmwire.ToolCall, 0, n)
	for i := range n {
		calls = append(calls, llmwire.ToolCall{
			ID:        fmt.Sprintf("read-%d", i),
			Name:      "read",
			Arguments: []byte(fmt.Sprintf(`{"path":"file-%d.go"}`, i)),
		})
	}

	return calls
}

// A read-heavy parallel fixture reaches the same final answer in fewer model
// iterations than its serial twin and records exactly one native tool_schedule
// summary with the decided field meanings. The fallback twin records one
// tool.batch summary instead.
func TestRunLoop_ReadHeavyFixtureFewerIterations(t *testing.T) {
	const reads = 5

	newReadAgent := func() *svc {
		agent := newTestAgent(&stubTool{id: "read", result: "file body", parallelSafe: true})

		return agent
	}

	// Parallel: one turn schedules all five reads, the next turn answers.
	parallelAgent := newReadAgent()
	parallelLLM := &loopScriptLLM{responses: []*llmwire.Response{
		{ToolCalls: readCalls(reads)},
		{Text: "same final answer"},
	}}
	parallelAgent.llmClient = parallelLLM

	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logger.ToContext(t.Context(), zap.New(core))

	result, err := runLoop(ctx, parallelAgent, loopOptions{}, iterationGuard(20))
	require.NoError(t, err)

	assert.Equal(t, "same final answer", result.FinalResponse)
	assert.Equal(t, 2, parallelLLM.calls, "one scheduling turn plus the answer turn")

	schedules := logs.FilterMessage("tool_schedule").All()
	require.Len(t, schedules, 1, "exactly one native tool_schedule summary")

	fields := schedules[0].ContextMap()
	assert.Equal(t, int64(reads), fields["calls"])
	assert.Equal(t, int64(1), fields["stages"], "five parallel-safe reads form one stage")
	// max_parallel is the observed peak overlap, bounded by the 4-slot window.
	assert.GreaterOrEqual(t, fields["max_parallel"], int64(1))
	assert.LessOrEqual(t, fields["max_parallel"], int64(4))
	assert.Equal(t, int64(reads), fields["executed"])
	assert.Equal(t, int64(0), fields["failed"])
	assert.Equal(t, int64(0), fields["skipped"])

	// Serial twin: the same five reads, one per turn.
	serialAgent := newReadAgent()
	serialResponses := make([]*llmwire.Response, 0, reads+1)
	for range reads {
		serialResponses = append(serialResponses, &llmwire.Response{ToolCalls: readCalls(1)})
	}
	serialResponses = append(serialResponses, &llmwire.Response{Text: "same final answer"})

	serialLLM := &loopScriptLLM{responses: serialResponses}
	serialAgent.llmClient = serialLLM

	serialCore, _ := observer.New(zapcore.InfoLevel)
	serialCtx := logger.ToContext(t.Context(), zap.New(serialCore))

	serialResult, err := runLoop(serialCtx, serialAgent, loopOptions{}, iterationGuard(20))
	require.NoError(t, err)

	assert.Equal(t, "same final answer", serialResult.FinalResponse)
	assert.Equal(t, reads+1, serialLLM.calls, "one call per turn costs one iteration each")
	assert.Less(t, parallelLLM.calls, serialLLM.calls, "the parallel fixture beats the serial one")
}

// The fallback twin of the read-heavy fixture: one batch call covering the same
// five reads, recording exactly one tool.batch summary.
func TestRunLoop_BatchFallbackFixtureRecordsOneSummary(t *testing.T) {
	agent := newTestAgent(&stubTool{id: "read", result: "file body", parallelSafe: true})
	agent.registry.Register(builtin.NewBatchTool(agent.registry))

	params := `{"calls":[`
	for i := range 5 {
		if i > 0 {
			params += ","
		}
		params += fmt.Sprintf(`{"tool":"read","params":{"path":"file-%d.go"}}`, i)
	}
	params += `]}`

	batchLLM := &loopScriptLLM{responses: []*llmwire.Response{
		{ToolCalls: []llmwire.ToolCall{{
			ID:        "batch-1",
			Name:      tool.IDBatch,
			Arguments: []byte(params),
		}}},
		{Text: "same final answer"},
	}}
	agent.llmClient = batchLLM

	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logger.ToContext(t.Context(), zap.New(core))

	result, err := runLoop(ctx, agent, loopOptions{}, iterationGuard(20))
	require.NoError(t, err)

	assert.Equal(t, "same final answer", result.FinalResponse)
	assert.Equal(t, 2, batchLLM.calls)

	batchSummaries := make([]any, 0)
	for _, entry := range logs.FilterMessage("tool_schedule").All() {
		if entry.LoggerName == "tool.batch" {
			batchSummaries = append(batchSummaries, entry)
		}
	}

	require.Len(t, batchSummaries, 1, "exactly one tool.batch summary")

	var fields map[string]any
	for _, entry := range logs.FilterMessage("tool_schedule").All() {
		if entry.LoggerName == "tool.batch" {
			fields = entry.ContextMap()
		}
	}

	assert.Equal(t, int64(5), fields["calls"])
	assert.Equal(t, int64(5), fields["executed"])
	assert.Equal(t, int64(0), fields["failed"])
}
