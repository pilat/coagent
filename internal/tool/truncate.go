package tool

const (
	MaxToolResultSize         = 25000
	MaxBrowserFrameSize       = 120000
	MaxToolResultContextShare = 0.3
)

// ToolResultBudgetForWindow caps output at limit characters or the configured
// share of the context window, whichever is smaller.
func ToolResultBudgetForWindow(contextWindowTokens, limit int) int {
	dynamicBudget := int(float64(contextWindowTokens) * 4 * MaxToolResultContextShare)
	if dynamicBudget < limit {
		return dynamicBudget
	}

	return limit
}
