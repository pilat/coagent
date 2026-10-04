package managerdiscovery

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestListModelsMapsEnrichedEntries(t *testing.T) {
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		Models: []config.ModelEntry{
			{
				ID:            "claude-opus-5",
				Name:          "Claude Opus 5",
				DisplayName:   "anthropic/Claude Opus 5",
				Pricing:       &config.ModelPricing{InputPrice: 5, OutputPrice: 25},
				Reasoning:     &config.ReasoningSpec{Supported: true, NativeEffort: true},
				EffortLevels:  []string{"low", "medium", "high", "max"},
				DefaultEffort: "medium",
			},
			{ID: "local/plain", Name: "Plain", DisplayName: "local/Plain"},
		},
	}}

	controller := New(nil, cfg, nil)

	result, err := controller.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, result.Models, 2)
	assert.Equal(t, "claude-opus-5", result.DefaultID)

	opus := result.Models[0]
	assert.Equal(t, "Claude Opus 5", opus.Name)
	assert.Equal(t, "anthropic/Claude Opus 5", opus.DisplayName)
	assert.InDelta(t, 5.0, opus.InputPrice, 1e-9)
	assert.InDelta(t, 25.0, opus.OutputPrice, 1e-9)
	assert.Equal(t, []string{"low", "medium", "high", "max"}, opus.EffortLevels)
	assert.Equal(t, "medium", opus.DefaultEffort)

	plain := result.Models[1]
	assert.Zero(t, plain.InputPrice)
	assert.Zero(t, plain.OutputPrice)
	assert.Empty(t, plain.EffortLevels, "a model with no catalog effort levels must not offer the step")
	assert.Empty(t, plain.DefaultEffort)

	// The reasoning fact alone is not the control: enrichment decides which levels
	// a driver can actually deliver, and it left this one with none.
	cfg.UnifiedConfig.Models[1].Reasoning = &config.ReasoningSpec{Supported: true}

	result, err = controller.ListModels(context.Background())
	require.NoError(t, err)
	assert.Empty(t, result.Models[1].EffortLevels)
}
