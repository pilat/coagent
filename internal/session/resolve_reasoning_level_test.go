package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

// The daemon settles a level with this before recording it and then hands the
// result to the live session, which settles it again — so the second pass must
// return the first pass unchanged, for every input shape.
func TestResolveReasoningLevelIsIdempotent(t *testing.T) {
	models := []config.ModelEntry{
		{ID: "plain"},
		{ID: "thinker", EffortLevels: []string{"low", "high"}, DefaultEffort: "high"},
	}

	tests := []struct {
		name      string
		model     string
		requested string
		want      string
	}{
		{name: "unnamed level lands on the model default", model: "thinker", requested: "", want: "high"},
		{name: "an accepted level is kept", model: "thinker", requested: "low", want: "low"},
		{name: "a model without efforts carries none", model: "plain", requested: "high", want: ""},
		{name: "and none is asked for either", model: "plain", requested: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveReasoningLevel(models, tt.model, tt.requested)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)

			again, err := ResolveReasoningLevel(models, tt.model, got)
			require.NoError(t, err)
			assert.Equal(t, got, again, "re-resolving a settled level must change nothing")
		})
	}
}

func TestResolveReasoningLevelRejectsWhatTheModelCannotRun(t *testing.T) {
	models := []config.ModelEntry{{ID: "thinker", EffortLevels: []string{"low", "high"}, DefaultEffort: "high"}}

	_, err := ResolveReasoningLevel(models, "thinker", "ultra")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not accept reasoning level")

	_, err = ResolveReasoningLevel(models, "ghost", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown model")
}
