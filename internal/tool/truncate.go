package tool

const (
	MaxToolResultSize         = 25000
	MaxBrowserFrameSize       = 120000
	MaxToolResultContextShare = 0.3
)

// DynamicToolResultBudgetForWindow computes the tool result truncation budget
// for a given context window size (in tokens).
// Returns the smaller of MaxToolResultSize and 30% of the context window (in chars).
func DynamicToolResultBudgetForWindow(contextWindowTokens int) int {
	return toolResultBudgetForWindow(contextWindowTokens, MaxToolResultSize)
}

// BrowserFrameBudgetForWindow caps one live browser frame at the usual window share.
func BrowserFrameBudgetForWindow(contextWindowTokens int) int {
	return toolResultBudgetForWindow(contextWindowTokens, MaxBrowserFrameSize)
}

func toolResultBudgetForWindow(contextWindowTokens, limit int) int {
	dynamicBudget := int(float64(contextWindowTokens) * 4 * MaxToolResultContextShare)
	if dynamicBudget < limit {
		return dynamicBudget
	}

	return limit
}
