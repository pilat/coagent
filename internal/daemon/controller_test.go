package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/schedule"
)

func TestListSchedulesMapsEntries(t *testing.T) {
	ctx := context.Background()
	mgr, _, store, schedStore := newTestManagerWithSchedule(t)

	projectID, err := store.GetOrCreateProject(ctx, "/p")
	require.NoError(t, err)
	rec, err := mgr.sessionStore.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)

	schedSvc := schedule.NewService(schedStore)
	_, err = schedSvc.AddRecurring(ctx, rec.ID, "CRON_TZ=Europe/Berlin 0 9 * * *", "morning job", true)
	require.NoError(t, err)

	oneShot := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	_, err = schedStore.AddSchedule(ctx, rec.ID, "", &oneShot, "wake once", false)
	require.NoError(t, err)

	controller := NewController(mgr, &config.Config{}, nil, schedSvc)
	result, err := controller.ListSchedules(ctx, controllerapi.ScheduleListData{SessionID: rec.ID})
	require.NoError(t, err)
	require.Len(t, result.Schedules, 2)

	var cron, once controllerapi.ScheduleInfo
	for _, s := range result.Schedules {
		if s.Cron != "" {
			cron = s
		} else {
			once = s
		}
	}

	assert.Equal(t, "0 9 * * *", cron.Cron, "CRON_TZ prefix stripped for display")
	assert.Equal(t, "Europe/Berlin", cron.Timezone)
	assert.True(t, cron.Fresh)
	assert.Equal(t, "morning job", cron.Prompt)
	assert.Nil(t, cron.OneShotAt)

	assert.Empty(t, once.Cron)
	assert.False(t, once.Fresh)
	require.NotNil(t, once.OneShotAt)
	assert.Equal(t, oneShot, once.OneShotAt.UTC())
}

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

	controller := NewController(nil, cfg, nil, nil)

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
