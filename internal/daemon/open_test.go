package daemon

import (
	"database/sql"
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

// TestResolveChildEffort covers the settling rules a spawn applies before the
// child's level is persisted, including the shape a brand-new session needs
// (nothing asked for, nothing inherited).
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

func TestSessionRepoRoot_InheritedAcrossDurableSubagentTree(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		"repo_root": "/source",
	})
	require.NoError(t, err)
	childID, err := func() (int64, error) {
		var id int64
		err := h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				h.ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       root.ID,
					RootID:         root.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	grandchildID, err := func() (int64, error) {
		var id int64
		err := h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				h.ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       childID,
					RootID:         root.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)

	for _, sessionID := range []int64{root.ID, childID, grandchildID} {
		rec, loadErr := h.store.GetSession(h.ctx, sessionID)
		require.NoError(t, loadErr)
		got, rootErr := h.mgr.sessionRepoRoot(h.ctx, rec)
		require.NoError(t, rootErr)
		assert.Equal(t, "/source", got)
	}

	child, err := h.store.GetSession(h.ctx, childID)
	require.NoError(t, err)
	assert.Empty(t, child.Attributes["repo_root"])

	plain, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	got, err := h.mgr.sessionRepoRoot(h.ctx, plain)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestSessionRepoRoot_RejectsCrossProjectRoot(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		"repo_root": "/source",
	})
	require.NoError(t, err)
	other := &sessionstore.SessionRecord{RootID: root.ID, ProjectID: h.projectID + 1}
	_, err = h.mgr.sessionRepoRoot(h.ctx, other)
	require.ErrorContains(t, err, "does not match project")
}
