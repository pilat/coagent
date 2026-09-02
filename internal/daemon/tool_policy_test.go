package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/configtools"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/tool"
)

// TestProductionTools_ParallelSafePolicies pins the concurrency declarations
// of every tool the daemon registers into session registries: task is the only
// parallel-safe one; any accidental opt-in or opt-out must fail here.
func TestProductionTools_ParallelSafePolicies(t *testing.T) {
	sp := &mockSpawner{}

	tools := map[string]tool.Tool{
		tool.IDTask:           newTaskTool(sp, 0, registry.NewSet(nil), nil),
		tool.IDSendToSubagent: newSendToSubagentTool(sp),
		"get_subagent_result": newGetSubagentResultTool(sp),
		tool.IDSchedule:       schedule.NewScheduleTool(0, nil, nil),
		tool.IDSleep:          schedule.NewSleepTool(nil, 0),
		budget.ToolID:         budget.NewTool(nil, 0, false),
		tool.IDRequestSecret:  &requestSecretTool{},
	}

	for _, tl := range configtools.New(nil, nil) {
		tools[tl.ID()] = tl
	}

	for _, tl := range newMCPTools(nil, nil, 0) {
		tools[tl.ID()] = tl
	}

	want := make(map[string]bool, len(tools))
	for id := range tools {
		want[id] = id == tool.IDTask
	}

	got := make(map[string]bool, len(tools))
	for id, tl := range tools {
		got[id] = tl.ParallelSafe()
	}

	assert.Equal(t, want, got)

	// The wait guard must not outvote its wrapped tool.
	guard := &subagentWaitGuard{inner: tools[tool.IDTask]}
	assert.True(t, guard.ParallelSafe())
	guardSleep := &subagentWaitGuard{inner: tools[tool.IDSleep]}
	assert.False(t, guardSleep.ParallelSafe())
}
