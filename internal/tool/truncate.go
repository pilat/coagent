package tool

const (
	MaxToolResultSize         = 25000
	MaxToolResultContextShare = 0.3
)

// DynamicToolResultBudgetForWindow computes the tool result truncation budget
// for a given context window size (in tokens).
// Returns the smaller of MaxToolResultSize and 30% of the context window (in chars).
func DynamicToolResultBudgetForWindow(contextWindowTokens int) int {
	dynamicBudget := int(float64(contextWindowTokens) * 4 * MaxToolResultContextShare)
	if dynamicBudget < MaxToolResultSize {
		return dynamicBudget
	}

	return MaxToolResultSize
}
