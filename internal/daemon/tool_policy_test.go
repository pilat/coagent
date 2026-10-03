package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

// TestProductionTools_ParallelSafePolicies pins the concurrency declarations
// of every tool the daemon registers into session registries: task is the only
// parallel-safe one; any accidental opt-in or opt-out must fail here.
func TestProductionTools_ParallelSafePolicies(t *testing.T) {
	tools := map[string]tool.Tool{
		tool.IDTask:           subagent.NewTaskTool(nil, 0, nil, nil),
		tool.IDSendToSubagent: subagent.NewSendToSubagentTool(nil),
		"get_subagent_result": subagent.NewGetSubagentResultTool(nil),
		tool.IDSchedule:       schedule.NewScheduleTool(0, nil, nil),
		tool.IDSleep:          schedule.NewSleepTool(nil, 0),
		budget.ToolID:         budget.NewTool(nil, 0, false),
	}

	for _, tl := range []tool.Tool{configapply.NewConfigEdit(0, nil)} {
		tools[tl.ID()] = tl
	}

	for _, tl := range mcpstore.NewTools(nil, 0) {
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

	assert.False(t, schedule.NewGuardedSleepTool(nil, 0, nil).ParallelSafe())
}
