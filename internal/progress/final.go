package progress

import (
	"strings"
	"time"
)

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
	// IsBackgroundYield marks a final the model released to a wake source: the
	// operator still has nothing to do, so the card keeps a badge. A confirmed
	// terminal stop stays badge-less — badge-less is the your-turn signal.
	IsBackgroundYield bool
}

// RenderFinalFromFacts appends the post-disposition progress footer to text.
// Pure: database-free, no I/O, no notifications. Replaces the store-reading
// FinalOutput path for responses committed through the disposition boundary.
func RenderFinalFromFacts(text string, facts FinalFacts) string {
	// The badge is a title line on the whole final, not a footer fragment:
	// the released message must open with "nothing for you to do".
	if facts.IsBackgroundYield {
		text = strings.TrimSpace("🟣 Background\n\n" + text)
	}

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

		// The badge was already applied to the whole text above; letting the
		// footer render it again would duplicate the title line.
		IsBackgroundYield: false,
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
