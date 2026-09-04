package progress

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const todoHint = "ℹ️ `/status` shows the full TODO list"

func RenderCompact(snapshot Snapshot, redact func(string) string) string {
	if redact == nil {
		redact = func(value string) string { return value }
	}

	lines := []string{"**" + cardTitle(snapshot) + "**"}

	if note := strings.TrimSpace(redact(snapshot.LatestModelProgress)); note != "" {
		lines = append(lines, "", note)
	}

	if snapshot.Model != "" {
		lines = append(lines, "", fmt.Sprintf("🤖 `%s` · iteration %d", snapshot.Model, snapshot.RootIteration))
	}

	if metrics := renderCardMetrics(snapshot); metrics != "" {
		lines = append(lines, metrics)
	}

	if subagents := renderSubagents(snapshot); subagents != "" {
		lines = append(lines, subagents)
	}

	if len(snapshot.Waiting) > 0 {
		lines = append(
			lines,
			fmt.Sprintf("⏳ Waiting on %d item%s", len(snapshot.Waiting), plural(len(snapshot.Waiting))),
		)
	}

	if todoBlock := renderCardTodos(snapshot.Todos); len(todoBlock) > 0 {
		lines = append(lines, todoBlock...)
	}

	if snapshot.Budget != nil {
		lines = append(lines, renderBudget(*snapshot.Budget))
	}

	return strings.Join(lines, "\n")
}

func RenderFull(snapshot Snapshot, redact func(string) string) string {
	if redact == nil {
		redact = func(value string) string { return value }
	}

	lines := []string{"## Session progress"}

	state := snapshot.RuntimeState
	if state == "" {
		state = "unavailable"
	}

	lines = append(lines, "- State: "+state)

	if snapshot.Model != "" {
		lines = append(lines, fmt.Sprintf("- Model: `%s` · root iteration %d", snapshot.Model, snapshot.RootIteration))
	}

	lines = append(lines, renderContext(snapshot.Context), renderUsage(snapshot.Lifetime))
	if snapshot.EpisodeElapsed != nil {
		lines = append(lines, "- Wall time: "+snapshot.EpisodeElapsed.Round(1e9).String())
	} else {
		lines = append(lines, "- Wall time: unavailable")
	}

	lines = append(lines, renderTodos(snapshot.Todos, redact)...)
	if note := strings.TrimSpace(redact(snapshot.LatestModelProgress)); note != "" {
		lines = append(lines, "- Latest agent note: "+note)
	}

	if len(snapshot.Waiting) > 0 {
		lines = append(lines, fmt.Sprintf("- Waiting: %d item(s)", len(snapshot.Waiting)))
	}

	if snapshot.ActiveSubagents > 0 {
		foreground := snapshot.ActiveSubagents - snapshot.BackgroundSubagents
		lines = append(lines, fmt.Sprintf(
			"- Active subagents: %d foreground · %d background",
			foreground,
			snapshot.BackgroundSubagents,
		))
	}

	if snapshot.Budget != nil {
		lines = append(lines, renderBudget(*snapshot.Budget))
	}

	lines = append(
		lines,
		fmt.Sprintf("- Children: %d · child iterations %d", snapshot.ChildCount, snapshot.ChildIterations),
		fmt.Sprintf(
			"- Observed: %s · revision `%s`",
			snapshot.ObservedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
			snapshot.Revision,
		),
	)

	return strings.Join(lines, "\n")
}

// RenderFooter is the final-output tail: TODO summaries only, then the budget
// line separated by one blank line. No TODO summary is produced when no list
// exists.
func RenderFooter(snapshot Snapshot, redact func(string) string) string {
	var parts []string
	if summary := renderTodoSummary(snapshot.Todos); summary != "" {
		parts = append(parts, summary)
	}

	if snapshot.Budget != nil {
		parts = append(parts, renderBudget(*snapshot.Budget))
	}

	return strings.Join(parts, "\n\n")
}

// A fired budget and an exact root wait outrank live work; inactivity is explicit.
func cardTitle(snapshot Snapshot) string {
	if snapshot.Budget != nil && snapshot.Budget.State == "fired" {
		return "🛑 Budget reached"
	}

	if len(snapshot.Waiting) > 0 {
		return "⏳ Waiting"
	}

	if snapshot.MainModelWorking {
		return "🟢 Working"
	}

	if snapshot.ActiveSubagents > 0 {
		return "🟣 Background work"
	}

	return "⚪ Idle"
}

// renderCardMetrics joins only the available fragments; the whole line
// disappears when nothing is measurable.
func renderCardMetrics(snapshot Snapshot) string {
	var fragments []string

	if snapshot.EpisodeElapsed != nil {
		fragments = append(fragments, "⌚ "+snapshot.EpisodeElapsed.Round(1e9).String())
	}

	if snapshot.Lifetime.Available && !math.IsNaN(snapshot.Lifetime.CostUSD) &&
		!math.IsInf(snapshot.Lifetime.CostUSD, 0) {
		fragments = append(fragments, "💰 $"+formatUSD(snapshot.Lifetime.CostUSD)+" total")
	}

	if snapshot.Context.Available && snapshot.Context.Max > 0 {
		prefix := ""
		if snapshot.Context.Approximate {
			prefix = "~"
		}

		percent := float64(snapshot.Context.Used) * 100 / float64(snapshot.Context.Max)
		fragments = append(fragments, fmt.Sprintf("🧠 context %s%.0f%%", prefix, percent))
	}

	return strings.Join(fragments, " · ")
}

func renderSubagents(snapshot Snapshot) string {
	if snapshot.ActiveSubagents == 0 {
		return ""
	}

	foreground := snapshot.ActiveSubagents - snapshot.BackgroundSubagents

	return fmt.Sprintf(
		"🧩 Subagents · %d foreground · %d background",
		foreground,
		snapshot.BackgroundSubagents,
	)
}

// renderCardTodos renders counts only — item text belongs to /status.
func renderCardTodos(items []TodoItem) []string {
	if len(items) == 0 {
		return nil
	}

	active, remaining, done, cancelled := Snapshot{Todos: items}.TodoCounts()

	line := fmt.Sprintf("📋 TODO · %d active · %d remaining · %d done", active, remaining, done)
	if cancelled > 0 {
		line += fmt.Sprintf(" · %d cancelled", cancelled)
	}

	return []string{line, todoHint}
}

// renderTodoSummary is the final-output TODO tail. An empty list appends
// nothing; finished work reads as success, unfinished work points at /status.
func renderTodoSummary(items []TodoItem) string {
	if len(items) == 0 {
		return ""
	}

	active, remaining, done, cancelled := Snapshot{Todos: items}.TodoCounts()

	if remaining == 0 {
		summary := fmt.Sprintf("✅ TODO complete · %d done", done)
		if cancelled > 0 {
			summary += fmt.Sprintf(" · %d cancelled", cancelled)
		}

		return summary
	}

	summary := fmt.Sprintf("📋 TODO · %d active · %d remaining · %d done", active, remaining, done)
	if cancelled > 0 {
		summary += fmt.Sprintf(" · %d cancelled", cancelled)
	}

	return summary + " · /status shows the full list"
}

func renderContext(value Context) string {
	if !value.Available || value.Max <= 0 {
		return "- Context: unavailable"
	}

	prefix := ""
	if value.Approximate {
		prefix = "~"
	}

	percent := float64(value.Used) * 100 / float64(value.Max)

	return fmt.Sprintf("- Context: %s%.0f%% (%d / %d tokens)", prefix, percent, value.Used, value.Max)
}

func renderUsage(value Usage) string {
	if !value.Available || math.IsNaN(value.CostUSD) || math.IsInf(value.CostUSD, 0) {
		return "- Lifetime usage: unavailable"
	}

	return fmt.Sprintf("- Persisted cost: $%.6f · %d prompt / %d completion tokens",
		value.CostUSD, value.PromptTokens, value.CompletionTokens)
}

func renderTodos(items []TodoItem, redact func(string) string) []string {
	if len(items) == 0 {
		return []string{"- TODO: no TODO is declared"}
	}

	active, remaining, done, cancelled := Snapshot{Todos: items}.TodoCounts()

	lines := []string{fmt.Sprintf("- TODO: %d active · %d remaining · %d done", active, remaining, done)}
	if cancelled > 0 {
		lines[0] += fmt.Sprintf(" · %d cancelled", cancelled)
	}

	for _, item := range items {
		lines = append(lines, fmt.Sprintf("  - [%s] %s", item.Status, redact(item.Content)))
	}

	return lines
}

func renderBudget(value Budget) string {
	line := fmt.Sprintf("- Budget: %s (generation %d)", value.State, value.Generation)
	if value.State == "fired" {
		return line + " · limiter is no longer armed · reason: " + value.FiredReason
	}

	if value.CostLimitUSD != nil && value.CostUsedUSD != nil {
		line += fmt.Sprintf(" · $%.6f / $%.6f", *value.CostUsedUSD, *value.CostLimitUSD)
		if value.CostRemainingUSD != nil {
			line += fmt.Sprintf(" · $%.6f remaining", *value.CostRemainingUSD)
		}
	}

	if value.DurationLimit != nil && value.Elapsed != nil {
		line += fmt.Sprintf(" · %s / %s", value.Elapsed.Round(1e9), value.DurationLimit.Round(1e9))
		if value.DurationRemaining != nil {
			line += " · " + value.DurationRemaining.Round(1e9).String() + " remaining"
		}
	}

	return line
}

// formatUSD renders at most six decimals with trailing zeroes trimmed while
// always keeping one decimal digit, so costs never read as integers.
func formatUSD(value float64) string {
	fixed := strings.TrimRight(strconv.FormatFloat(value, 'f', 6, 64), "0")
	if strings.HasSuffix(fixed, ".") {
		return fixed + "0"
	}

	return fixed
}

func plural(count int) string {
	if count == 1 {
		return ""
	}

	return "s"
}
