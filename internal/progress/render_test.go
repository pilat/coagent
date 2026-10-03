package progress

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/logger"
)

func TestRenderCompact_TruthfulFallbackAndRedaction(t *testing.T) {
	logger.SetRedactedValues([]string{"secret-value"})
	t.Cleanup(func() { logger.SetRedactedValues(nil) })

	snapshot := Snapshot{
		RuntimeState:        "running",
		Context:             Context{Available: false},
		Lifetime:            Usage{Available: false},
		Todos:               []TodoItem{{ID: "a", Content: "inspect secret-value", Status: "pending"}},
		LatestModelProgress: "using secret-value",
	}

	rendered := RenderCompact(snapshot)
	assert.NotContains(t, rendered, "secret-value")
	assert.Contains(t, rendered, "[REDACTED]")
}

func TestRenderCompact_ExactCard(t *testing.T) {
	t.Parallel()

	elapsed := 96 * time.Second
	cost := 0.281
	snapshot := Snapshot{
		Model:               "z-ai/glm-5.3-flash",
		RootIteration:       112,
		ChildCount:          2,
		ChildIterations:     9,
		MainModelWorking:    true,
		EpisodeElapsed:      &elapsed,
		Lifetime:            Usage{Available: true, CostUSD: cost},
		Context:             Context{Available: true, Used: 72, Max: 100},
		LatestModelProgress: "reading the loop",
		Todos: []TodoItem{
			{ID: "1", Content: "only in status", Status: "in_progress"},
			{ID: "2", Content: "pending", Status: "pending"},
			{ID: "3", Content: "done", Status: "completed"},
			{ID: "4", Content: "done two", Status: "completed"},
			{ID: "5", Content: "gone", Status: "cancelled"},
		},
	}

	assert.Equal(t, strings.Join([]string{
		"**🟢 Working**",
		"",
		"reading the loop",
		"",
		"🤖 `z-ai/glm-5.3-flash` · iteration 121",
		"⌚ 1m36s · 💰 $0.281 total · 🧠 context 72%",
		"📋 TODO · 1 active · 2 remaining · 2 done · 1 cancelled",
		"ℹ️ /status shows the full TODO list",
	}, "\n"), RenderCompact(snapshot))
}

func TestRenderCompact_ShowsActiveSubagentsByMode(t *testing.T) {
	t.Parallel()

	snapshot := Snapshot{ActiveSubagents: 3, BackgroundSubagents: 1}
	rendered := RenderCompact(snapshot)

	assert.Contains(t, rendered, "🧩 Subagents · 2 foreground · 1 background")

	assert.NotContains(t, RenderCompact(Snapshot{}), "Subagents")
}

func TestCardTitle_Table(t *testing.T) {
	t.Parallel()

	waiting := Snapshot{Waiting: []WaitingItem{{Kind: "sleep"}}}
	processes := Snapshot{BackgroundProcesses: []ProcessStatus{{ProcessID: "p1"}}}

	cases := []struct {
		name     string
		snapshot Snapshot
		want     string
	}{
		{"waiting, model idle", waiting, "🟣 Background"},
		{"background processes, model idle", processes, "🟣 Background"},
		{"active subagents", Snapshot{ActiveSubagents: 1}, "🟣 Background"},
		{"working outranks background processes", Snapshot{
			MainModelWorking:    true,
			BackgroundProcesses: []ProcessStatus{{ProcessID: "p1"}},
		}, "🟢 Working"},
		{"budget fired outranks all", func() Snapshot {
			s := processes
			s.Budget = &Budget{State: "fired", Generation: 1}
			return s
		}(), "🛑 Budget reached"},
		{"nothing in flight", Snapshot{}, "⚪ Idle"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cardTitle(tc.snapshot))
		})
	}
}

func TestRenderCompact_TitlePrecedence(t *testing.T) {
	t.Parallel()

	fired := &Budget{State: "fired", Generation: 1}
	armed := &Budget{State: "armed", Generation: 1}

	waiting := Snapshot{Waiting: []WaitingItem{{Kind: "sleep"}}}
	assert.Equal(t, "**🟣 Background**", RenderCompact(waiting))
	assert.Equal(t, "**⚪ Idle**", RenderCompact(Snapshot{}))
	assert.Equal(t, "**🟢 Working**", RenderCompact(Snapshot{MainModelWorking: true}))
	assert.Equal(t, strings.Join([]string{
		"**🟣 Background**",
		"🧩 Subagents · 0 foreground · 1 background",
	}, "\n"), RenderCompact(Snapshot{ActiveSubagents: 1, BackgroundSubagents: 1}))

	firedWaiting := waiting
	firedWaiting.Budget = fired
	assert.Contains(t, RenderCompact(firedWaiting), "**🛑 Budget reached**")

	armedWaiting := waiting
	armedWaiting.Budget = armed
	assert.Contains(t, RenderCompact(armedWaiting), "**🟣 Background**")

	assert.Contains(t, RenderCompact(Snapshot{Budget: armed}), "**⚪ Idle**")
}

// The compact card communicates waiting through the 🟣 title only; item counts
// belong to /status.
func TestRenderCompact_NoWaitingDetailLine(t *testing.T) {
	t.Parallel()

	one := Snapshot{Waiting: []WaitingItem{{Kind: "sleep"}}}
	rendered := RenderCompact(one)
	assert.NotContains(t, rendered, "⏳")
	assert.NotContains(t, rendered, "Waiting on")
}

func TestRenderCompact_MissingFragmentsOmitted(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "**⚪ Idle**", RenderCompact(Snapshot{}))

	approximate := Snapshot{
		Model:         "m",
		RootIteration: 3,
		Context:       Context{Available: true, Used: 7, Max: 100, Approximate: true},
	}
	assert.Equal(t, strings.Join([]string{
		"**⚪ Idle**",
		"",
		"🤖 `m` · iteration 3",
		"🧠 context ~7%",
	}, "\n"), RenderCompact(approximate))

	noModel := Snapshot{EpisodeElapsed: new(time.Minute)}
	assert.Equal(t, strings.Join([]string{
		"**⚪ Idle**",
		"⌚ 1m0s",
	}, "\n"), RenderCompact(noModel))
}

func TestRenderCompact_USDTrimming(t *testing.T) {
	t.Parallel()

	zero := Snapshot{Lifetime: Usage{Available: true, CostUSD: 0}}
	assert.Contains(t, RenderCompact(zero), "💰 $0.0 total")

	precise := Snapshot{Lifetime: Usage{Available: true, CostUSD: 12}}
	assert.Contains(t, RenderCompact(precise), "💰 $12.0 total")

	sixDecimals := Snapshot{Lifetime: Usage{Available: true, CostUSD: 0.123456789}}
	assert.Contains(t, RenderCompact(sixDecimals), "💰 $0.123457 total")
}

// /status keeps its diagnostic waiting count even though the compact card no
// longer renders it.
func TestRenderFull_KeepsWaitingCountLine(t *testing.T) {
	t.Parallel()

	snapshot := Snapshot{Waiting: []WaitingItem{{Kind: "sleep"}}}
	assert.Contains(t, RenderFull(snapshot), "- Waiting: 1 item(s)")
}

func TestRenderCompact_UnboundedNoteBeyond512Runes(t *testing.T) {
	logger.SetRedactedValues([]string{"secret-value"})
	t.Cleanup(func() { logger.SetRedactedValues(nil) })

	note := strings.Repeat("界", 700) + " secret-value"
	snapshot := Snapshot{LatestModelProgress: note}

	rendered := RenderCompact(snapshot)
	assert.Contains(t, rendered, strings.Repeat("界", 700))
	assert.Contains(t, rendered, "[REDACTED]")
	assert.NotContains(t, rendered, "…")
}

func TestRenderCompact_NoTODOBlockForEmptyList(t *testing.T) {
	t.Parallel()

	rendered := RenderCompact(Snapshot{})
	assert.NotContains(t, rendered, "TODO")
	assert.NotContains(t, rendered, "/status")
}

func TestRenderCompact_BudgetDetailBelowTODOBlock(t *testing.T) {
	t.Parallel()

	snapshot := Snapshot{
		Todos:  []TodoItem{{ID: "1", Content: "x", Status: "pending"}},
		Budget: &Budget{State: "fired", Generation: 2, FiredReason: "cost"},
	}
	rendered := RenderCompact(snapshot)

	assert.Equal(t, strings.Join([]string{
		"**🛑 Budget reached**",
		"📋 TODO · 0 active · 1 remaining · 0 done",
		"ℹ️ /status shows the full TODO list",
		"💸 Budget: fired (generation 2) · limiter is no longer armed · reason: cost",
	}, "\n"), rendered)
}

func TestRenderFull_KeepsDiagnosticsAndFullNote(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	note := strings.Repeat("界", 600)
	snapshot := Snapshot{
		ObservedAt:          now,
		Revision:            "rev",
		RuntimeState:        "running",
		Model:               "m",
		RootIteration:       5,
		ChildCount:          2,
		ChildIterations:     9,
		Context:             Context{Available: true, Used: 1000, Max: 8000},
		Lifetime:            Usage{Available: true, PromptTokens: 10, CompletionTokens: 20, CostUSD: 0.5},
		EpisodeElapsed:      new(time.Minute),
		LatestModelProgress: note,
		Todos: []TodoItem{
			{ID: "1", Content: "ship change", Status: "in_progress"},
			{ID: "2", Content: "mystery", Status: "weird"},
		},
		Waiting: []WaitingItem{{Kind: "sleep"}},
	}

	rendered := RenderFull(snapshot)
	assert.Contains(t, rendered, "- State: running")
	assert.Contains(t, rendered, "- Model: `m` · root iteration 5")
	assert.Contains(t, rendered, "- Context: 12% (1000 / 8000 tokens)")
	assert.Contains(t, rendered, "- Persisted cost: $0.500000 · 10 prompt / 20 completion tokens")
	assert.Contains(t, rendered, "- Wall time: 1m0s")
	assert.Contains(t, rendered, "- TODO: 1 active · 2 remaining · 0 done")
	assert.Contains(t, rendered, "  - 🔄 ship change")
	assert.Contains(t, rendered, "  - ❔ mystery")
	assert.Contains(t, rendered, "Legend: ⏳ pending · 🔄 in progress · ✅ completed · 🚫 cancelled")
	assert.Contains(t, rendered, "- Latest agent note: "+note)
	assert.Contains(t, rendered, "- Waiting: 1 item(s)")
	assert.Contains(t, rendered, "- Children: 2 · child iterations 9")
	assert.Contains(t, rendered, "- Observed: 2026-08-29 12:00:00 UTC · revision `rev`")
}

// Icon-only rows use the exact shape `  - <emoji> <content>`; the legend follows
// one blank line after the rows and disappears with the list itself.
func TestRenderFull_TodoRowsAndLegend(t *testing.T) {
	logger.SetRedactedValues([]string{"secret-value"})
	t.Cleanup(func() { logger.SetRedactedValues(nil) })

	empty := RenderFull(Snapshot{})
	assert.Contains(t, empty, "- TODO: no TODO is declared")
	assert.NotContains(t, empty, "Legend:")
	assert.NotContains(t, empty, "⏳")

	snapshot := Snapshot{
		Todos: []TodoItem{
			{ID: "1", Content: "inspect secret-value", Status: "pending"},
			{ID: "2", Content: "ship change", Status: "in_progress"},
			{ID: "3", Content: "done deal", Status: "completed"},
			{ID: "4", Content: "gone", Status: "cancelled"},
			{ID: "5", Content: "mystery", Status: "weird"},
		},
	}

	assert.Equal(t, strings.Join([]string{
		"## Session progress",
		"- State: unavailable",
		"- Context: unavailable",
		"- Lifetime usage: unavailable",
		"- Wall time: unavailable",
		"- TODO: 1 active · 3 remaining · 1 done · 1 cancelled",
		"  - ⏳ inspect [REDACTED]",
		"  - 🔄 ship change",
		"  - ✅ done deal",
		"  - 🚫 gone",
		"  - ❔ mystery",
		"",
		"Legend: ⏳ pending · 🔄 in progress · ✅ completed · 🚫 cancelled",
		"- Children: 0 · child iterations 0",
		"- Observed: 0001-01-01 00:00:00 UTC · revision ``",
	}, "\n"), RenderFull(snapshot))
}

func TestRenderFooter_SummariesOnly(t *testing.T) {
	t.Parallel()

	// No list: nothing at all.
	assert.Empty(t, RenderFooter(Snapshot{}))

	// Finished work, with cancellations.
	complete := RenderFooter(Snapshot{
		Todos: []TodoItem{
			{ID: "1", Status: "completed"},
			{ID: "2", Status: "completed"},
			{ID: "3", Status: "cancelled"},
		},
	})
	assert.Equal(t, "✅ TODO complete · 2 done · 1 cancelled", complete)

	// Unfinished work points at /status.
	unfinished := RenderFooter(Snapshot{
		Todos: []TodoItem{
			{ID: "1", Status: "in_progress"},
			{ID: "2", Status: "pending"},
			{ID: "3", Status: "completed"},
		},
	})
	assert.Equal(t, "📋 TODO · 1 active · 2 remaining · 1 done · /status shows the full list", unfinished)

	// Budget detail separated by one blank line.
	both := RenderFooter(Snapshot{
		Todos:  []TodoItem{{ID: "1", Status: "completed"}},
		Budget: &Budget{State: "armed", Generation: 1},
	})
	assert.Equal(t, "✅ TODO complete · 1 done\n\n💸 Budget: armed (generation 1)", both)

	budgetOnly := RenderFooter(Snapshot{Budget: &Budget{State: "armed", Generation: 1}})
	assert.Equal(t, "💸 Budget: armed (generation 1)", budgetOnly)
}
