package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestBuildThinkingParamsDisabled(t *testing.T) {
	tests := []struct {
		name string
		spec *config.ReasoningSpec
	}{
		{name: "no catalog spec", spec: nil},
		{name: "model does not reason", spec: &config.ReasoningSpec{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildThinkingParams(tt.spec, ReasoningHigh, 64000)
			assert.False(t, got.Enabled)
		})
	}
}

func TestBuildThinkingParamsNativeEffort(t *testing.T) {
	spec := &config.ReasoningSpec{Supported: true, NativeEffort: true, BudgetMin: 1024}

	for _, level := range []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh} {
		t.Run(string(level), func(t *testing.T) {
			got := buildThinkingParams(spec, level, 64000)

			require.True(t, got.Enabled)
			assert.NotNil(t, got.Thinking.OfAdaptive, "effort-native models take adaptive thinking")
			assert.Nil(t, got.Thinking.OfEnabled, "a budget must not be sent alongside effort")
			assert.Equal(t, string(level), string(got.Effort))
		})
	}
}

func TestBuildThinkingParamsBudget(t *testing.T) {
	tests := []struct {
		name        string
		level       ReasoningLevel
		budgetMin   int
		maxTokens   int
		wantEnabled bool
		wantBudget  int64
	}{
		{
			name:        "low is a fifth of the output limit",
			level:       ReasoningLow,
			budgetMin:   1024,
			maxTokens:   64000,
			wantEnabled: true,
			wantBudget:  12800,
		},
		{
			name:        "medium is half",
			level:       ReasoningMedium,
			budgetMin:   1024,
			maxTokens:   64000,
			wantEnabled: true,
			wantBudget:  32000,
		},
		{
			name:        "high is four fifths",
			level:       ReasoningHigh,
			budgetMin:   1024,
			maxTokens:   64000,
			wantEnabled: true,
			wantBudget:  51200,
		},
		{
			name:        "xhigh is nearly all of it",
			level:       ReasoningXHigh,
			budgetMin:   1024,
			maxTokens:   64000,
			wantEnabled: true,
			wantBudget:  60800,
		},
		{
			name:        "an unknown level falls back to medium",
			level:       "turbo",
			budgetMin:   1024,
			maxTokens:   64000,
			wantEnabled: true,
			wantBudget:  32000,
		},
		{
			name:        "the catalog floor lifts a small fraction",
			level:       ReasoningLow,
			budgetMin:   8000,
			maxTokens:   10000,
			wantEnabled: true,
			wantBudget:  8000,
		},
		{
			name:        "the ceiling caps the floor",
			level:       ReasoningHigh,
			budgetMin:   4000,
			maxTokens:   4001,
			wantEnabled: true,
			wantBudget:  4000,
		},
		{
			name:        "inverted bounds skip thinking entirely",
			level:       ReasoningHigh,
			budgetMin:   1024,
			maxTokens:   1000,
			wantEnabled: false,
		},
		{
			name:        "no room at all skips thinking",
			level:       ReasoningMedium,
			budgetMin:   1024,
			maxTokens:   1,
			wantEnabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := &config.ReasoningSpec{Supported: true, BudgetMin: tt.budgetMin}

			got := buildThinkingParams(spec, tt.level, tt.maxTokens)
			require.Equal(t, tt.wantEnabled, got.Enabled)

			if !tt.wantEnabled {
				return
			}

			require.NotNil(t, got.Thinking.OfEnabled, "budget models take enabled thinking")
			assert.Nil(t, got.Thinking.OfAdaptive)
			assert.Empty(t, string(got.Effort), "budget models take no effort field")
			assert.Equal(t, tt.wantBudget, got.Thinking.OfEnabled.BudgetTokens)
			assert.Less(t, got.Thinking.OfEnabled.BudgetTokens, int64(tt.maxTokens),
				"the API rejects a budget at or above max_tokens")
		})
	}
}

func TestBuildMessageParamsSendsThinking(t *testing.T) {
	tests := []struct {
		name         string
		spec         *config.ReasoningSpec
		level        string
		wantAdaptive bool
		wantBudget   int64
		wantEffort   string
	}{
		{
			name:         "effort-native model",
			spec:         &config.ReasoningSpec{Supported: true, NativeEffort: true},
			level:        "high",
			wantAdaptive: true,
			wantEffort:   "high",
		},
		{
			name:       "budget model",
			spec:       &config.ReasoningSpec{Supported: true, BudgetMin: 1024},
			level:      "low",
			wantBudget: 12800,
		},
		{
			name:  "non-reasoning model sends nothing",
			spec:  &config.ReasoningSpec{},
			level: "high",
		},
		{
			name:       "unset level defaults to medium",
			spec:       &config.ReasoningSpec{Supported: true, BudgetMin: 1024},
			wantBudget: 32000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &anthropicClient{
				model:          "claude-test",
				maxTokens:      64000,
				reasoning:      tt.spec,
				reasoningLevel: ReasoningLevel(tt.level),
			}

			params := c.buildMessageParams("sys", nil, nil, c.maxTokens)

			assert.Equal(t, tt.wantAdaptive, params.Thinking.OfAdaptive != nil)
			assert.Equal(t, tt.wantEffort, string(params.OutputConfig.Effort))

			if tt.wantBudget == 0 {
				assert.Nil(t, params.Thinking.OfEnabled)
				return
			}

			require.NotNil(t, params.Thinking.OfEnabled)
			assert.Equal(t, tt.wantBudget, params.Thinking.OfEnabled.BudgetTokens)
		})
	}
}
