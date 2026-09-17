package progress

import "time"

// FinalFacts is the closed post-disposition input to the pure final renderer:
// the confirmed response's iteration, usage, budget verdict, and todo state,
// plus the transaction timestamp. No store access, no I/O.
type FinalFacts struct {
	Model            string
	Iteration        int
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	TodoItems        []TodoItem
	Budget           *Budget
	ObservedAt       time.Time
}

// RenderFinalFromFacts appends the post-disposition progress footer to text.
// Pure: database-free, no I/O, no notifications. Replaces the store-reading
// FinalOutput path for responses committed through the disposition boundary.
func RenderFinalFromFacts(text string, facts FinalFacts) string {
	snapshot := Snapshot{
		Model:         facts.Model,
		RootIteration: facts.Iteration,
		Lifetime: Usage{
			PromptTokens:     facts.PromptTokens,
			CompletionTokens: facts.CompletionTokens,
			CostUSD:          facts.CostUSD,
			Available: facts.PromptTokens != 0 || facts.CompletionTokens != 0 ||
				facts.CostUSD != 0,
		},
		Todos:      facts.TodoItems,
		Budget:     facts.Budget,
		ObservedAt: facts.ObservedAt,
	}

	footer := RenderFinalCompact(snapshot)
	if footer == "" {
		return text
	}

	if text == "" {
		return footer
	}

	return text + "\n\n" + footer
}
