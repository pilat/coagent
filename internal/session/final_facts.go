package session

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
)

// finalFactsFromProgress adapts durable progress facts captured inside the
// response transaction into the pure renderer input. Iteration and usage
// already include the just-committed assistant row.
func finalFactsFromProgress(
	facts *sessionstore.ProgressFacts,
	observedAt time.Time,
) (progress.FinalFacts, error) {
	items, err := finalTodoItems(facts)
	if err != nil {
		return progress.FinalFacts{}, err
	}

	out := progress.FinalFacts{
		Model:            facts.Model,
		Iteration:        facts.Iteration,
		PromptTokens:     facts.PromptTokens,
		CompletionTokens: facts.CompletionTokens,
		CostUSD:          facts.CostUSD,
		TodoItems:        items,
		ObservedAt:       observedAt,
	}
	if facts.Budget != nil {
		out.Budget = finalBudget(facts.Budget, facts.CostUSD, observedAt)
	}

	return out, nil
}

func finalTodoItems(facts *sessionstore.ProgressFacts) ([]progress.TodoItem, error) {
	var items []*todo.Item
	if err := json.Unmarshal(facts.TodoItems, &items); err != nil {
		return nil, fmt.Errorf("decode final todo: %w", err)
	}

	todo.SortCanonical(items)

	out := make([]progress.TodoItem, 0, len(items))
	for _, item := range items {
		out = append(out, progress.TodoItem{
			ID: item.ID, Content: item.Content,
			Status: string(item.Status), Priority: string(item.Priority),
		})
	}

	return out, nil
}

func finalBudget(
	record *sessionstore.BudgetRecord,
	cost float64,
	now time.Time,
) *progress.Budget {
	value := &progress.Budget{
		State: string(record.State), Generation: record.Generation,
		CostLimitUSD: record.CostLimitUSD, FiredReason: record.FiredReason,
	}
	used := cost - record.BaselineCostUSD

	value.CostUsedUSD = &used
	if record.CostLimitUSD != nil {
		remaining := max(*record.CostLimitUSD-used, 0)
		value.CostRemainingUSD = &remaining
	}

	if record.DurationSeconds != nil {
		limit := time.Duration(*record.DurationSeconds) * time.Second
		value.DurationLimit = &limit

		if !now.Before(record.ArmedAt) {
			elapsed := now.Sub(record.ArmedAt)
			value.Elapsed = &elapsed
			remaining := max(limit-elapsed, 0)
			value.DurationRemaining = &remaining
		}
	}

	return value
}
