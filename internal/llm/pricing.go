package llm

import (
	"github.com/pilat/coagent/internal/config"
)

// estimateCost prices a call from the model's catalog-resolved rates. A model the
// catalog carries no cost for is reported at zero — an honest zero beats a guess.
func estimateCost(usage Usage, pricing *config.ModelPricing) float64 {
	if pricing == nil {
		return 0
	}

	effectiveInput := max(0, usage.PromptTokens-usage.CacheTokens-usage.CacheWriteTokens)
	inputCost := float64(effectiveInput) * pricing.InputPrice / 1_000_000
	outputCost := float64(usage.CompletionTokens) * pricing.OutputPrice / 1_000_000
	cacheReadCost := float64(usage.CacheTokens) * pricing.CacheReadPrice / 1_000_000
	cacheWriteCost := float64(usage.CacheWriteTokens) * pricing.CacheWritePrice / 1_000_000

	return inputCost + outputCost + cacheReadCost + cacheWriteCost
}
