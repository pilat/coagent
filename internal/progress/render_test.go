package progress

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRenderCompact_TruthfulFallbackAndRedaction(t *testing.T) {
	t.Parallel()

	snapshot := Snapshot{
		RuntimeState:        "running",
		Context:             Context{Available: false},
		Lifetime:            Usage{Available: false},
		Todos:               []TodoItem{{ID: "a", Content: "inspect secret-value", Status: "pending"}},
		LatestModelProgress: "using secret-value",
	}

	rendered := RenderCompact(snapshot, func(value string) string {
		return strings.ReplaceAll(value, "secret-value", "[REDACTED]")
	})
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
	}, "\n"), RenderCompact(snapshot, nil))
}

func TestRenderCompact_ShowsActiveSubagentsByMode(t *testing.T) {
	t.Parallel()

	snapshot := Snapshot{ActiveSubagents: 3, BackgroundSubagents: 1}
	rendered := RenderCompact(snapshot, nil)

	assert.Contains(t, rendered, "🧩 Subagents · 2 foreground · 1 background")

	assert.NotContains(t, RenderCompact(Snapshot{}, nil), "Subagents")
}

func TestRenderCompact_TitlePrecedence(t *testing.T) {
	t.Parallel()

	fired := &Budget{State: "fired", Generation: 1}
	armed := &Budget{State: "armed", Generation: 1}

	waiting := Snapshot{Waiting: []WaitingItem{{Kind: "sleep"}}}
	assert.Contains(t, RenderCompact(waiting, nil), "**⏳ Waiting**")
	assert.Equal(t, "**⚪ Idle**", RenderCompact(Snapshot{}, nil))
	assert.Equal(t, "**🟢 Working**", RenderCompact(Snapshot{MainModelWorking: true}, nil))
	assert.Equal(t, strings.Join([]string{
		"**🟣 Background work**",
		"🧩 Subagents · 0 foreground · 1 background",
	}, "\n"), RenderCompact(Snapshot{ActiveSubagents: 1, BackgroundSubagents: 1}, nil))

	firedWaiting := waiting
	firedWaiting.Budget = fired
	assert.Contains(t, RenderCompact(firedWaiting, nil), "**🛑 Budget reached**")

	armedWaiting := waiting
	armedWaiting.Budget = armed
	assert.Contains(t, RenderCompact(armedWaiting, nil), "**⏳ Waiting**")

	assert.Contains(t, RenderCompact(Snapshot{Budget: armed}, nil), "**⚪ Idle**")
}

func TestRenderCompact_MissingFragmentsOmitted(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "**⚪ Idle**", RenderCompact(Snapshot{}, nil))

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
	}, "\n"), RenderCompact(approximate, nil))

	noModel := Snapshot{EpisodeElapsed: new(time.Minute)}
	assert.Equal(t, strings.Join([]string{
		"**⚪ Idle**",
		"⌚ 1m0s",
	}, "\n"), RenderCompact(noModel, nil))
}

func TestRenderShieldsOnlyWhileRaised(t *testing.T) {
	raised := Snapshot{ShieldsUp: true}
	assert.Contains(t, RenderCompact(raised, nil), "🛡️ Shields raised")
	assert.Contains(t, RenderFull(raised, nil), "- Shields: raised")

	assert.NotContains(t, RenderCompact(Snapshot{}, nil), "Shields")
	assert.NotContains(t, RenderFull(Snapshot{}, nil), "Shields")
}

func TestRenderCompact_USDTrimming(t *testing.T) {
	t.Parallel()

	zero := Snapshot{Lifetime: Usage{Available: true, CostUSD: 0}}
	assert.Contains(t, RenderCompact(zero, nil), "💰 $0.0 total")

	precise := Snapshot{Lifetime: Usage{Available: true, CostUSD: 12}}
	assert.Contains(t, RenderCompact(precise, nil), "💰 $12.0 total")

	sixDecimals := Snapshot{Lifetime: Usage{Available: true, CostUSD: 0.123456789}}
	assert.Contains(t, RenderCompact(sixDecimals, nil), "💰 $0.123457 total")
}

func TestRenderCompact_WaitingSingularAndPlural(t *testing.T) {
	t.Parallel()

	one := Snapshot{Waiting: []WaitingItem{{Kind: "sleep"}}}
	assert.Contains(t, RenderCompact(one, nil), "⏳ Waiting on 1 item")

	many := Snapshot{Waiting: []WaitingItem{{Kind: "sleep"}, {Kind: "subagent"}}}
	assert.Contains(t, RenderCompact(many, nil), "⏳ Waiting on 2 items")
}

func TestRenderCompact_UnboundedNoteBeyond512Runes(t *testing.T) {
	t.Parallel()

	note := strings.Repeat("界", 700) + " secret-value"
	snapshot := Snapshot{LatestModelProgress: note}

	rendered := RenderCompact(snapshot, func(value string) string {
		return strings.ReplaceAll(value, "secret-value", "[REDACTED]")
	})
	assert.Contains(t, rendered, strings.Repeat("界", 700))
	assert.Contains(t, rendered, "[REDACTED]")
	assert.NotContains(t, rendered, "…")
}

func TestRenderCompact_NoTODOBlockForEmptyList(t *testing.T) {
	t.Parallel()

	rendered := RenderCompact(Snapshot{}, nil)
	assert.NotContains(t, rendered, "TODO")
	assert.NotContains(t, rendered, "/status")
}

func TestRenderCompact_BudgetDetailBelowTODOBlock(t *testing.T) {
	t.Parallel()

	snapshot := Snapshot{
		Todos:  []TodoItem{{ID: "1", Content: "x", Status: "pending"}},
		Budget: &Budget{State: "fired", Generation: 2, FiredReason: "cost"},
	}
	rendered := RenderCompact(snapshot, nil)

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

	rendered := RenderFull(snapshot, nil)
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
	t.Parallel()

	empty := RenderFull(Snapshot{}, nil)
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
	}, "\n"), RenderFull(snapshot, func(value string) string {
		return strings.ReplaceAll(value, "secret-value", "[REDACTED]")
	}))
}

func TestRenderFooter_SummariesOnly(t *testing.T) {
	t.Parallel()

	// No list: nothing at all.
	assert.Empty(t, RenderFooter(Snapshot{}, nil))

	// Finished work, with cancellations.
	complete := RenderFooter(Snapshot{
		Todos: []TodoItem{
			{ID: "1", Status: "completed"},
			{ID: "2", Status: "completed"},
			{ID: "3", Status: "cancelled"},
		},
	}, nil)
	assert.Equal(t, "✅ TODO complete · 2 done · 1 cancelled", complete)

	// Unfinished work points at /status.
	unfinished := RenderFooter(Snapshot{
		Todos: []TodoItem{
			{ID: "1", Status: "in_progress"},
			{ID: "2", Status: "pending"},
			{ID: "3", Status: "completed"},
		},
	}, nil)
	assert.Equal(t, "📋 TODO · 1 active · 2 remaining · 1 done · /status shows the full list", unfinished)

	// Budget detail separated by one blank line.
	both := RenderFooter(Snapshot{
		Todos:  []TodoItem{{ID: "1", Status: "completed"}},
		Budget: &Budget{State: "armed", Generation: 1},
	}, nil)
	assert.Equal(t, "✅ TODO complete · 1 done\n\n💸 Budget: armed (generation 1)", both)

	budgetOnly := RenderFooter(Snapshot{Budget: &Budget{State: "armed", Generation: 1}}, nil)
	assert.Equal(t, "💸 Budget: armed (generation 1)", budgetOnly)
}
