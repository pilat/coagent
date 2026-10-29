package assistant

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssistantSwitch(t *testing.T) {
	t.Parallel()
	f := newToolFixture(t)
	for _, ref := range []string{`0`, `"0"`, `"#0"`} {
		_, err := f.tools[toolSwitch].Execute(t.Context(), json.RawMessage(`{"session":`+ref+`}`))
		require.EqualError(t, err, "switch: you cannot switch the user to Assistant; they return with /assistant")
	}
	foreign := workSession(t, f.sessions, map[string]any{"manager_id": "other"})
	for _, id := range []int64{foreign, f.root} {
		_, err := f.tools[toolSwitch].Execute(t.Context(), json.RawMessage(fmt.Sprintf(`{"session":%d}`, id)))
		require.ErrorContains(t, err, "not a work session")
	}
	assert.Empty(t, f.host.focused)
	require.NoError(t, f.sessions.BindChat(t.Context(), testOwner, "chat"))
	assert.Equal(t, fmt.Sprintf("switched the user to session %d; they return with /assistant", f.work),
		f.execute(t, toolSwitch, fmt.Sprintf(`{"session":"#%d"}`, f.work)))
	assert.Equal(t, []int64{f.work}, f.host.focused)
}

func TestAssistantModels(t *testing.T) {
	t.Parallel()
	f := newToolFixture(t)
	f.host.models = []Model{
		{
			ID:            "first",
			DisplayName:   "First model",
			EffortLevels:  []string{"low", "high"},
			DefaultEffort: "low",
			Reasoning:     true,
			Priced:        true,
			InputPrice:    1,
			OutputPrice:   4,
		},
		{ID: "second"},
	}
	assert.Equal(t, "first · First model · default · efforts: low, high · default effort: low · reasoning · $1/$4\n"+
		"second · second · no effort choice · unpriced", f.execute(t, toolModels, `{}`))
	assert.True(t, f.tools[toolModels].ParallelSafe())
	assert.False(t, f.tools[toolSwitch].ParallelSafe())
	assert.False(t, f.tools[toolSetModel].ParallelSafe())
}

func TestAssistantSetModel(t *testing.T) {
	for _, tc := range []struct {
		name, model, effort, want string
		self                      bool
	}{
		{name: "default", model: "reasoner"},
		{name: "explicit", model: "reasoner", effort: `,"effort":"high"`},
		{name: "plain", model: "plain"},
		{name: "unknown", model: "missing", want: "unknown model: missing"},
		{name: "bad effort", model: "reasoner", effort: `,"effort":"max"`, want: "allowed effort levels for reasoner: low, high"},
		{name: "empty effort", model: "reasoner", effort: `,"effort":""`, want: "allowed effort levels for reasoner: low, high"},
		{name: "no efforts", model: "plain", effort: `,"effort":"high"`, want: "allowed effort levels for plain: none"},
		{name: "self", model: "reasoner", self: true, want: "not a work session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newToolFixture(t)
			f.host.models = []Model{
				{ID: "reasoner", DisplayName: "Reasoner", EffortLevels: []string{"low", "high"}, DefaultEffort: "low"},
				{ID: "plain", DisplayName: "Plain"},
			}
			target := f.work
			if tc.self {
				target = f.root
			}
			result, err := f.tools[toolSetModel].Execute(t.Context(), json.RawMessage(fmt.Sprintf(
				`{"session":%d,"model":%q%s}`, target, tc.model, tc.effort)))
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
				assert.Empty(t, f.host.modelCalls)
				return
			}
			require.NoError(t, err)
			require.Len(t, f.host.modelCalls, 1)
			effort, name := "low", "Reasoner"
			if tc.effort != "" {
				effort = "high"
			}
			if tc.model == "plain" {
				effort, name = "", "Plain"
			}
			assert.Equal(t, effort, *f.host.modelCalls[0].Effort)
			want := fmt.Sprintf("session %d now runs %s", f.work, name)
			if effort != "" {
				want += " · " + effort
			}
			assert.Equal(t, want, result.Output)
		})
	}
}
