package progress

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/logger"
)

func TestRenderFull_KeepsWaitingCountLine(t *testing.T) {
	t.Parallel()

	snapshot := Snapshot{Waiting: []WaitingItem{{Kind: "sleep"}}}
	assert.Contains(t, RenderFull(snapshot), "- Waiting: 1 item(s)")
}

func TestRenderFull_KeepsDiagnostics(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	snapshot := Snapshot{
		ObservedAt:      now,
		Revision:        "rev",
		RuntimeState:    "running",
		Model:           "m",
		RootIteration:   5,
		ChildCount:      2,
		ChildIterations: 9,
		Context:         Context{Available: true, Used: 1000, Max: 8000},
		Lifetime:        Usage{Available: true, PromptTokens: 10, CompletionTokens: 20, CostUSD: 0.5},
		EpisodeElapsed:  new(time.Minute),
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
	assert.Contains(t, rendered, "- Waiting: 1 item(s)")
	assert.Contains(t, rendered, "- Children: 2 · child iterations 9")
	assert.Contains(t, rendered, "- Observed: 2026-08-29 12:00:00 UTC · revision `rev`")
}

func TestRenderFull_PreservesLiteralCodeValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		value  string
		quoted string
	}{
		{name: "plain", value: "value/v1", quoted: "`value/v1`"},
		{name: "backtick", value: "a`b", quoted: "`a`b`"},
		{name: "newline", value: "a\nb", quoted: "`a\nb`"},
		{name: "controls", value: "a\r\t\x00b", quoted: "`a\r\t\x00b`"},
		{name: "invalid UTF-8", value: "a\xffb", quoted: "`a\xffb`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rendered := RenderFull(Snapshot{Model: tt.value, Revision: tt.value, RootIteration: 5})
			assert.Contains(t, rendered, "- Model: "+tt.quoted+" · root iteration 5\n")
			assert.True(t, strings.HasSuffix(rendered, " · revision "+tt.quoted))
		})
	}
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
