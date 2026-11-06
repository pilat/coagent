package progress

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/logger"
)

func RenderFull(snapshot Snapshot) string {
	lines := []string{"## Session progress"}

	state := snapshot.RuntimeState
	if state == "" {
		state = "unavailable"
	}

	lines = append(lines, "- State: "+state)

	if snapshot.Model != "" {
		lines = append(lines, fmt.Sprintf(
			"- Model: %s · root iteration %d", "`"+snapshot.Model+"`", snapshot.RootIteration,
		))
	}

	lines = append(lines, renderContext(snapshot.Context), renderUsage(snapshot.Lifetime))
	if snapshot.EpisodeElapsed != nil {
		lines = append(lines, "- Wall time: "+snapshot.EpisodeElapsed.Round(1e9).String())
	} else {
		lines = append(lines, "- Wall time: unavailable")
	}

	lines = append(lines, renderTodos(snapshot.Todos)...)
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

	lines = append(lines, renderProcesses(snapshot.BackgroundProcesses)...)

	if snapshot.Budget != nil {
		lines = append(lines, renderBudget(*snapshot.Budget))
	}

	lines = append(
		lines,
		fmt.Sprintf("- Children: %d · child iterations %d", snapshot.ChildCount, snapshot.ChildIterations),
		fmt.Sprintf(
			"- Observed: %s · revision %s",
			snapshot.ObservedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
			"`"+snapshot.Revision+"`",
		),
	)

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

func renderTodos(items []TodoItem) []string {
	if len(items) == 0 {
		return []string{"- TODO: no TODO is declared"}
	}

	active, remaining, done, cancelled := Snapshot{Todos: items}.TodoCounts()

	lines := []string{fmt.Sprintf("- TODO: %d active · %d remaining · %d done", active, remaining, done)}
	if cancelled > 0 {
		lines[0] += fmt.Sprintf(" · %d cancelled", cancelled)
	}

	for _, item := range items {
		lines = append(lines, fmt.Sprintf("  - %s %s", todoStatusIcon(item.Status), logger.Redact(item.Content)))
	}

	lines = append(lines, "", "Legend: ⏳ pending · 🔄 in progress · ✅ completed · 🚫 cancelled")

	return lines
}

func todoStatusIcon(status string) string {
	switch status {
	case "pending":
		return "⏳"
	case "in_progress":
		return "🔄"
	case "completed":
		return "✅"
	case "cancelled":
		return "🚫"
	default:
		return "❔"
	}
}

func renderBudget(value Budget) string {
	line := fmt.Sprintf("💸 Budget: %s (generation %d)", value.State, value.Generation)
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

// renderProcesses projects live background Bash processes. Only stable
// identities, state, and bounded counters appear here — never command text.
func renderProcesses(processes []ProcessStatus) []string {
	if len(processes) == 0 {
		return nil
	}

	lines := []string{fmt.Sprintf("- Background processes: %d running", len(processes))}

	for _, process := range processes {
		deadline := process.DeadlineAt.UTC().Format("15:04:05")
		lines = append(lines, fmt.Sprintf(
			"  - %s · owner %s · %s · elapsed %s · deadline %s UTC · output %s",
			process.ProcessID, process.Owner, process.State,
			process.Elapsed.Truncate(time.Second), deadline,
			humanBytes(process.OutputSize),
		))
	}

	return lines
}

func humanBytes(size int64) string {
	const unit = 1024

	switch {
	case size >= unit*unit*unit:
		return fmt.Sprintf("%.1f GiB", float64(size)/(unit*unit*unit))
	case size >= unit*unit:
		return fmt.Sprintf("%.1f MiB", float64(size)/(unit*unit))
	case size >= unit:
		return fmt.Sprintf("%.1f KiB", float64(size)/unit)
	default:
		return fmt.Sprintf("%d B", size)
	}
}
