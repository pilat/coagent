package progress

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

const latestExcerptRunes = 512

func RenderCompact(snapshot Snapshot, redact func(string) string) string {
	return render(snapshot, redact, false)
}

func RenderFull(snapshot Snapshot, redact func(string) string) string {
	return render(snapshot, redact, true)
}

func RenderFooter(snapshot Snapshot, redact func(string) string) string {
	if redact == nil {
		redact = func(value string) string { return value }
	}

	var lines []string
	if len(snapshot.Todos) > 0 {
		lines = renderTodos(snapshot.Todos, redact)
	}

	if snapshot.Budget != nil {
		lines = append(lines, renderBudget(*snapshot.Budget))
	}

	return strings.Join(lines, "\n")
}

func render(snapshot Snapshot, redact func(string) string, full bool) string {
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
	if excerpt := excerpt(redact(snapshot.LatestModelProgress)); excerpt != "" {
		lines = append(lines, "- Latest agent note: "+excerpt)
	}

	if len(snapshot.Waiting) > 0 {
		lines = append(lines, fmt.Sprintf("- Waiting: %d item(s)", len(snapshot.Waiting)))
	}

	if snapshot.Budget != nil {
		lines = append(lines, renderBudget(*snapshot.Budget))
	}

	if full {
		lines = append(
			lines,
			fmt.Sprintf("- Children: %d · child iterations %d", snapshot.ChildCount, snapshot.ChildIterations),
			fmt.Sprintf(
				"- Observed: %s · revision `%s`",
				snapshot.ObservedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
				snapshot.Revision,
			),
		)
	}

	return strings.Join(lines, "\n")
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

	current, completed, remaining := Snapshot{Todos: items}.TodoCounts()

	lines := []string{fmt.Sprintf("- TODO: %d current · %d completed · %d remaining", current, completed, remaining)}
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

func excerpt(value string) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= latestExcerptRunes {
		return value
	}

	runes := []rune(value)

	return string(runes[:latestExcerptRunes]) + "…"
}
