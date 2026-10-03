package session

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// A read-heavy parallel fixture reaches the same final answer in fewer model
// iterations than its serial twin and records exactly one native tool_schedule
// summary with the decided field meanings. The fallback twin records one
// tool.batch summary instead.
func TestRunLoop_ReadHeavyFixtureFewerIterations(t *testing.T) {
	const reads = 5

	newReadAgent := func() *Session {
		agent := newTestAgent(&stubTool{id: "read", result: "file body", parallelSafe: true})
		setTestMessages(agent, []llmwire.Message{usr("task")})

		return agent
	}

	// Parallel: one turn schedules all five reads, the next turn answers.
	parallelAgent := newReadAgent()
	parallelLLM := &loopScriptLLM{responses: []*llmwire.Response{
		{ToolCalls: readCalls(reads)},
		{Text: "same final answer"},
		{Text: "confirmed"},
	}}
	parallelAgent.llmClient = parallelLLM

	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logger.ToContext(t.Context(), zap.New(core))

	result, err := parallelAgent.Run(ctx)
	require.NoError(t, err)

	assert.Equal(t, "same final answer", result.Final)
	assert.Equal(t, 3, parallelLLM.calls, "one scheduling turn plus answer and confirmation")

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
	serialResponses = append(serialResponses, &llmwire.Response{Text: "confirmed"})

	serialLLM := &loopScriptLLM{responses: serialResponses}
	serialAgent.llmClient = serialLLM

	serialCore, _ := observer.New(zapcore.InfoLevel)
	serialCtx := logger.ToContext(t.Context(), zap.New(serialCore))

	serialResult, err := serialAgent.Run(serialCtx)
	require.NoError(t, err)

	assert.Equal(t, "same final answer", serialResult.Final)
	assert.Equal(t, reads+2, serialLLM.calls, "one call per turn plus answer and confirmation")
	assert.Less(t, parallelLLM.calls, serialLLM.calls, "the parallel fixture beats the serial one")
}

// The fallback twin of the read-heavy fixture: one batch call covering the same
// five reads, recording exactly one tool.batch summary.
func TestRunLoop_BatchFallbackFixtureRecordsOneSummary(t *testing.T) {
	agent := newTestAgent(&stubTool{id: "read", result: "file body", parallelSafe: true})
	setTestMessages(agent, []llmwire.Message{usr("task")})
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
		{Text: "confirmed"},
	}}
	agent.llmClient = batchLLM

	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logger.ToContext(t.Context(), zap.New(core))

	result, err := agent.Run(ctx)
	require.NoError(t, err)

	assert.Equal(t, "same final answer", result.Final)
	assert.Equal(t, 3, batchLLM.calls)

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

func TestRun_DurableTurns(t *testing.T) {
	tests := []struct {
		name, input string
		responses   []*llmwire.Response
		tools       []tool.Tool
		calls       int
		final       string
		status      sessionstore.SessionStatus
		wantError   bool
		ownerless   bool
	}{
		{
			name:      "confirmed answer",
			input:     "answer the request",
			responses: []*llmwire.Response{textResponse("candidate"), textResponse("confirmed")},
			calls:     2,
			final:     "candidate",
			status:    sessionstore.SessionStatusCompleted,
		},
		{
			name:      "ownerless observer answer",
			input:     "answer the request",
			responses: []*llmwire.Response{textResponse("candidate"), textResponse("confirmed")},
			calls:     2,
			final:     "candidate",
			status:    sessionstore.SessionStatusCompleted,
			ownerless: true,
		},
		{
			name:  "ordinary tool then confirmation",
			input: "read a file",
			responses: []*llmwire.Response{
				toolCallResponse("read-1", "read"),
				textResponse("candidate"),
				textResponse("confirmed"),
			},
			tools:  []tool.Tool{&stubTool{id: "read", result: "body"}},
			calls:  3,
			final:  "candidate",
			status: sessionstore.SessionStatusCompleted,
		},
		{
			name:      "owned sleep suspends",
			input:     "wait",
			responses: []*llmwire.Response{toolCallResponse("sleep-1", tool.IDSleep)},
			tools:     []tool.Tool{&stubTool{id: tool.IDSleep, err: tool.ErrSuspend}},
			calls:     1,
			status:    sessionstore.SessionStatusSuspended,
		},
		{
			name:  "empty stop breaks after six",
			input: "answer",
			responses: []*llmwire.Response{
				textResponse(""),
				textResponse(""),
				textResponse(""),
				textResponse(""),
				textResponse(""),
				textResponse(""),
			},
			calls:  6,
			status: sessionstore.SessionStatusError,
		},
		{
			name:      "unknown finish retains cost",
			input:     "answer",
			responses: []*llmwire.Response{{Text: "partial", FinishType: llmwire.FinishUnknown, CostUSD: 0.25}},
			calls:     1,
			status:    sessionstore.SessionStatusError,
			wantError: true,
		},
		{
			name:  "length retry retains rejected cost",
			input: "answer",
			responses: []*llmwire.Response{
				{Text: "partial", FinishType: llmwire.FinishLength, CostUSD: 0.25},
				textResponse("candidate"),
				textResponse("confirmed"),
			},
			calls:  3,
			final:  "candidate",
			status: sessionstore.SessionStatusCompleted,
		},
		{name: "status does not call model", input: "/status", calls: 0, status: sessionstore.SessionStatusCompleted},
		{
			name:   "unknown skill rejection does not call model",
			input:  "/skill missing",
			calls:  0,
			status: sessionstore.SessionStatusCompleted,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, store, id := newAttachmentsStore(t)
			if tc.ownerless {
				require.NoError(t, store.SetAttributes(t.Context(), id, nil))
			}
			_, err := store.Enqueue(
				t.Context(),
				sessionstore.Input{SessionID: id, Source: sessionstore.InputSourceUser, Content: tc.input},
			)
			require.NoError(t, err)
			record, err := store.GetSession(t.Context(), id)
			require.NoError(t, err)
			client := &loopScriptLLM{responses: tc.responses}
			reg := tool.NewRegistry()
			for _, tt := range tc.tools {
				reg.Register(tt)
			}
			prompt := sessionprompt.NewBuilder("stable system prompt", "")
			prompt.WorkDir = t.TempDir()
			prompt.Todos = todo.New()
			var notes []string
			s, err := New(
				t.Context(),
				Input{
					Record:         record,
					Client:         client,
					Loader:         loader.New(),
					Registry:       reg,
					Prompt:         prompt,
					Store:          store,
					Events:         noteEvents{notes: &notes},
					OpeningContext: "project instructions",
					OutputEnabled:  !tc.ownerless,
				},
			)
			require.NoError(t, err)
			t.Cleanup(s.Close)
			result, err := s.Run(t.Context())
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.calls, client.calls)
			assert.Equal(t, tc.final, result.Final)
			record, err = store.GetSession(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, tc.status, record.Status)
			pending, err := store.ListPending(t.Context(), id)
			require.NoError(t, err)
			assert.Empty(t, pending)
			if tc.status == sessionstore.SessionStatusSuspended {
				assert.True(t, result.Suspended)
				assert.Equal(t, []PendingToolCall{{ID: "sleep-1", Name: tool.IDSleep}}, s.PendingExternalCalls())
			}
			if tc.final != "" {
				assert.Equal(t, 1, countNotes(notes, tc.final), "candidate appears only after confirmation")
				assert.Contains(t, notes, tc.final, "live observers receive the raw answer")
				reloaded, err := New(
					t.Context(),
					Input{
						Record:        record,
						Client:        &loopScriptLLM{},
						Loader:        loader.New(),
						Registry:      reg,
						Prompt:        prompt,
						Store:         store,
						Events:        noteEvents{notes: &notes},
						OutputEnabled: !tc.ownerless,
					},
				)
				require.NoError(t, err)
				t.Cleanup(reloaded.Close)
				_, err = reloaded.Run(t.Context())
				require.NoError(t, err)
				assert.Equal(t, 1, countNotes(notes, tc.final), "reconstruction does not publish historical output")
			}
			if tc.name == "unknown finish retains cost" || tc.name == "length retry retains rejected cost" {
				_, _, cost, err := store.GetSessionTreeUsage(context.Background(), id)
				require.NoError(t, err)
				assert.InDelta(t, 0.25, cost, 0.000001)
			}
		})
	}
}

func TestLastAssistantTextOnly(t *testing.T) {
	tests := []struct {
		name     string
		messages []llmwire.Message
		want     string
	}{
		{
			name:     "empty history",
			messages: []llmwire.Message{},
			want:     "",
		},
		{
			name: "last message is assistant with no tool calls",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "hello"},
				{Role: llmwire.RoleAssistant, Content: "how can I help?"},
			},
			want: "how can I help?",
		},
		{
			name: "last message is assistant WITH tool calls",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "run something"},
				{
					Role:      llmwire.RoleAssistant,
					Content:   "sure",
					ToolCalls: []llmwire.ToolCall{{ID: "tc1", Name: "bash"}},
				},
			},
			want: "",
		},
		{
			name: "last message is user",
			messages: []llmwire.Message{
				{Role: llmwire.RoleAssistant, Content: "previous reply"},
				{Role: llmwire.RoleUser, Content: "follow-up question"},
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := lastAssistantTextOnly(tc.messages)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLastUserMessage(t *testing.T) {
	tests := []struct {
		name     string
		messages []llmwire.Message
		want     string
	}{
		{
			name:     "empty history",
			messages: []llmwire.Message{},
			want:     "",
		},
		{
			name: "last message is user",
			messages: []llmwire.Message{
				{Role: llmwire.RoleAssistant, Content: "previous reply"},
				{Role: llmwire.RoleUser, Content: "my latest question"},
			},
			want: "my latest question",
		},
		{
			name: "last message is assistant",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "hello"},
				{Role: llmwire.RoleAssistant, Content: "hi there"},
			},
			want: "hello",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := lastUserMessage(tc.messages)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestBuildSessionStatus_LifetimeFromTreeOccupancyFromProjection verifies lifetime
// comes from the DB tree-sum (survives compaction, keeps climbing) while occupancy
// is the compaction trigger's own projection, denominated by the same window source.
func TestBuildSessionStatus_LifetimeFromTreeOccupancyFromProjection(t *testing.T) {
	mockLLM := &compactionMockLLM{contextWindow: 200000}
	store := &statusStubStore{in: 500000, out: 50000, cost: 12.34, subagents: 2}

	s := newCompactionTestSvc(mockLLM)
	s.store = store
	s.ms = newMessageStore(store, 1)
	s.rootID = 1
	s.model = "test-model"
	setTestMessages(s, []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleAssistant, Content: "big turn"},
	})
	s.storeContextBaseline(150000, 2, s.modelGeneration())

	st := s.buildSessionStatus(context.Background())

	assert.Equal(t, 500000, st.LifetimeIn)
	assert.Equal(t, 50000, st.LifetimeOut)
	assert.InDelta(t, 12.34, st.LifetimeCost, 1e-9)
	assert.Equal(t, 150000, st.ContextUsed, "occupancy is the measured baseline plus its (empty) tail")
	assert.False(t, st.ContextIsEst, "a provider measurement backs it")
	assert.Equal(t, 200000, st.ContextMax, "denominator is s.contextWindow(), not a literal")
	assert.Equal(t, 2, st.SubagentCount)

	// A new message after the measurement is counted as a len/4 delta on top.
	require.NoError(t, appendTestUser(context.Background(), s.ms, strings.Repeat("x", 4000)))

	grown := s.buildSessionStatus(context.Background())
	assert.Equal(t, 151000, grown.ContextUsed, "baseline plus the tail estimate")
	assert.False(t, grown.ContextIsEst)
}

// After a compaction the baseline is gone, so /status falls back to a whole-
// transcript estimate — marked as one, and never 0%.
func TestBuildSessionStatus_AfterCompactionEstimatesAndIsNotZero(t *testing.T) {
	mockLLM := &compactionMockLLM{contextWindow: 200000}

	s := newCompactionTestSvc(mockLLM)
	store := &statusStubStore{in: 600000, out: 50000, cost: 15.0}
	s.store = store
	s.ms = newMessageStore(store, 1)
	s.rootID = 1
	setTestMessages(s, []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "[CONTEXT SUMMARY - previous work condensed] " + strings.Repeat("b", 4000)},
	})
	s.resetContextBaseline()

	st := s.buildSessionStatus(context.Background())

	assert.True(t, st.ContextIsEst, "no measurement survives a compaction")
	assert.Positive(t, st.ContextUsed, "0% right after a compaction would lie in the dangerous direction")
	assert.Contains(t, renderStatus(st), "~", "an estimate is visibly marked")
}

func TestRenderStatus_BandsAndBar(t *testing.T) {
	// Fresh session (no assistant turn) → 0%, green, empty bar.
	fresh := renderStatus(sessionStatus{Model: "m", ContextMax: 200000})
	assert.Contains(t, fresh, "🟢")
	assert.Contains(t, fresh, "0%")
	assert.Contains(t, fresh, "`░░░░░░░░░░`")

	// 90% → red + compacting soon; round(90/10)=9 filled cells.
	red := renderStatus(sessionStatus{Model: "m", ContextUsed: 180000, ContextMax: 200000})
	assert.Contains(t, red, "🔴")
	assert.Contains(t, red, "compacting soon")
	assert.Contains(t, red, "`█████████░`")
	assert.Contains(t, red, "90%")

	// 75% → yellow, no compacting-soon tail; round(75/10)=8 filled cells.
	yellow := renderStatus(sessionStatus{Model: "m", ContextUsed: 150000, ContextMax: 200000})
	assert.Contains(t, yellow, "🟡")
	assert.NotContains(t, yellow, "compacting soon")
	assert.Contains(t, yellow, "`████████░░`")

	// Exactly 85% is the red cut (compactionFraction).
	edge := renderStatus(sessionStatus{Model: "m", ContextUsed: 170000, ContextMax: 200000})
	assert.Contains(t, edge, "🔴")
}

func TestRenderStatus_CostHeadlineAndTokenSuffixes(t *testing.T) {
	out := renderStatus(sessionStatus{
		Model: "m", LifetimeCost: 12.3456, LifetimeIn: 2_500_000, LifetimeOut: 40000, ContextMax: 200000,
	})

	assert.Contains(t, out, "**$12.35**", "cost is bold, 2 decimals")
	assert.Contains(t, out, "all-in")
	assert.Contains(t, out, "2.5M in")
	assert.Contains(t, out, "40.0k out")
}
