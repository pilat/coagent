package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/config"
)

func TestEstimateCost(t *testing.T) {
	tests := []struct {
		name    string
		usage   Usage
		pricing *config.ModelPricing
		want    float64
	}{
		{
			name:    "no catalog pricing bills at zero",
			usage:   Usage{PromptTokens: 1_000_000, CompletionTokens: 100_000},
			pricing: nil,
			want:    0,
		},
		{
			name:    "input and output",
			usage:   Usage{PromptTokens: 1_000_000, CompletionTokens: 100_000},
			pricing: &config.ModelPricing{InputPrice: 2.50, OutputPrice: 10.00},
			want:    2.50 + 1.00,
		},
		{
			name: "cache tokens are billed at their own rates and excluded from input",
			usage: Usage{
				PromptTokens:     1_000_000,
				CompletionTokens: 100_000,
				CacheTokens:      400_000,
				CacheWriteTokens: 100_000,
			},
			pricing: &config.ModelPricing{
				InputPrice:      3.00,
				OutputPrice:     15.00,
				CacheReadPrice:  0.30,
				CacheWritePrice: 3.75,
			},
			// 500k effective input, 400k cache read, 100k cache write
			want: 1.50 + 1.50 + 0.12 + 0.375,
		},
		{
			name:    "zero-cost catalog entry",
			usage:   Usage{PromptTokens: 1_000_000, CompletionTokens: 100_000},
			pricing: &config.ModelPricing{},
			want:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.InDelta(t, tt.want, estimateCost(tt.usage, tt.pricing), 0.0001)
		})
	}
}
