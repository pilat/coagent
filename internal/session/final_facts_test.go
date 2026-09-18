package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestFinalFactsFromProgress_IncludesIterationUsageAndTodo(t *testing.T) {
	now := time.Now().UTC()
	limit := 10.0

	facts, err := finalFactsFromProgress(&sessionstore.ProgressFacts{
		Model: "test-model", Iteration: 7,
		PromptTokens: 1000, CompletionTokens: 200, CostUSD: 0.5,
		TodoItems: []byte(`[{"id":"1","content":"done work","status":"completed","priority":"high"},
			{"id":"2","content":"open work","status":"pending","priority":"medium"}]`),
		Budget: &sessionstore.BudgetRecord{
			State: sessionstore.BudgetArmed, Generation: 1, CostLimitUSD: &limit, BaselineCostUSD: 0.1,
		},
	}, now)
	require.NoError(t, err)

	rendered := progress.RenderFinalFromFacts("confirmed answer", facts)
	assert.Contains(t, rendered, "confirmed answer")
	assert.Contains(t, rendered, "iteration 7")
	assert.Contains(t, rendered, "TODO")
	assert.Contains(t, rendered, "Budget")
}

func TestFinalFactsFromProgress_RejectsBadTodo(t *testing.T) {
	_, err := finalFactsFromProgress(&sessionstore.ProgressFacts{
		TodoItems: []byte(`not json`),
	}, time.Now().UTC())
	require.Error(t, err)
}
