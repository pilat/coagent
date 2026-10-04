package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

func TestModelHasPricingRequiresMatchingPricedEntry(t *testing.T) {
	t.Parallel()
	manager := &svc{models: models{entries: []config.ModelEntry{
		{ID: "unpriced"},
		{ID: "priced", Pricing: &config.ModelPricing{}},
	}}}
	assert.False(t, manager.models.priced("missing"))
	assert.False(t, manager.models.priced("unpriced"))
	assert.True(t, manager.models.priced("priced"))
}

// Spawn must settle child effort before persistence, including an uninherited default for a new session.
func TestResolveChildEffort(t *testing.T) {
	entries := []config.ModelEntry{
		reasoningModelEntry("parent-model", []string{"low", "high"}, "high"),
		reasoningModelEntry("child-model", []string{"low", "medium"}, "low"),
		{ID: "plain-model", Provider: "or"},
	}
	tests := []struct {
		name      string
		entries   []config.ModelEntry
		model     string
		requested string
		inherited string
		want      string
		wantErr   string
	}{
		{
			name:    "nothing asked or inherited lands on the model default",
			entries: entries, model: "child-model", want: "low",
		},
		{
			name:    "an inherited level the model offers is kept",
			entries: entries, model: "child-model", inherited: "medium", want: "medium",
		},
		{
			name:    "an inherited level the model does not offer falls back to its default",
			entries: entries, model: "child-model", inherited: "high", want: "low",
		},
		{
			name:    "an asked-for level the model offers wins over the inherited one",
			entries: entries, model: "child-model", requested: "medium", inherited: "low", want: "medium",
		},
		{
			name:    "an asked-for level the model rejects fails the spawn",
			entries: entries, model: "child-model", requested: "high",
			wantErr: "does not accept reasoning level",
		},
		{
			name:    "a model with no effort selector carries no level",
			entries: entries, model: "plain-model", inherited: "high", want: "",
		},
		{
			name:    "an unknown model fails the spawn",
			entries: entries, model: "ghost", wantErr: "unknown model",
		},
		{
			name:  "no catalog vouches for nothing, so the level passes through",
			model: "child-model", inherited: "high", want: "high",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &svc{models: models{entries: tt.entries}}
			got, err := s.models.effort(tt.model, tt.requested, tt.inherited)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Only task is parallel-safe among daemon-registered tools; accidental policy changes must fail this test.
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

func TestSessionRepoRoot_InheritedAcrossDurableSubagentTree(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root := h.createRoot(map[string]any{"repo_root": "/source"})
	childID := h.createUnlinkedChild(root)
	grandchildID := h.createUnlinkedChild(childID)
	for _, sessionID := range []int64{root, childID, grandchildID} {
		rec := h.session(sessionID)
		got, rootErr := h.mgr.sessionRepoRoot(h.ctx, rec)
		require.NoError(t, rootErr)
		assert.Equal(t, "/source", got)
	}
	child := h.session(childID)
	assert.Empty(t, child.Attributes["repo_root"])
	plain, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	got, err := h.mgr.sessionRepoRoot(h.ctx, plain)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestSessionRepoRoot_RejectsCrossProjectRoot(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	var err error
	root := h.createRoot(map[string]any{"repo_root": "/source"})
	other := &sessionstore.SessionRecord{RootID: root, ProjectID: h.projectID + 1}
	_, err = h.mgr.sessionRepoRoot(h.ctx, other)
	require.ErrorContains(t, err, "does not match project")
}
