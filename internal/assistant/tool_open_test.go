package assistant

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssistantOpenWorktreeNeedsExistingProject(t *testing.T) {
	t.Parallel()
	f := newToolFixture(t)
	_, err := f.tools[toolOpen].Execute(t.Context(), json.RawMessage(
		`{"project":"missing","about":"work","text":"start","worktree":"feat-x"}`))
	require.EqualError(t, err, "open: worktree needs an existing project: missing")
	assert.Empty(t, f.host.created)
	assert.NoDirExists(t, filepath.Join(f.store.projectsRoot, "missing"))
}

func TestAssistantOpenModel(t *testing.T) {
	for _, tc := range []struct{ name, args, model, effort, want string }{
		{"explicit", `,"model":"reasoner","effort":"high"`, "reasoner", "high", ""},
		{"model default", `,"model":"reasoner"`, "reasoner", "low", ""},
		{"effort only", `,"effort":"high"`, "reasoner", "high", ""},
		{"plain", `,"model":"plain"`, "plain", "medium", ""},
		{"unknown", `,"model":"missing"`, "", "", "unknown model: missing"},
		{"bad effort", `,"model":"reasoner","effort":"max"`, "", "", "allowed effort levels for reasoner: low, high"},
		{"empty effort", `,"effort":""`, "", "", "allowed effort levels for reasoner: low, high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newToolFixture(t)
			f.host.models = []Model{
				{ID: "reasoner", EffortLevels: []string{"low", "high"}, DefaultEffort: "low"},
				{ID: "plain"},
			}
			_, err := f.tools[toolOpen].Execute(t.Context(), json.RawMessage(
				`{"project":"devbox","about":"work","text":"start"`+tc.args+`}`))
			if tc.want != "" {
				require.EqualError(t, err, "open: "+tc.want)
				assert.Empty(t, f.host.created)
				assert.Empty(t, f.host.deliveries)
				return
			}
			require.NoError(t, err)
			require.Len(t, f.host.created, 1)
			record, err := f.sessions.GetSession(t.Context(), f.host.created[0])
			require.NoError(t, err)
			assert.Equal(t, tc.model, record.Model)
			assert.Equal(t, tc.effort, record.ReasoningLevel)
		})
	}
}
