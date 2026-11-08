package builtin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBrowserFrameRule(t *testing.T) {
	t.Parallel()

	frames := (*Stack)(nil).Frames()
	require.NotNil(t, frames)
	assert.Equal(t, 120000, frames.ResultLimit())

	for _, tt := range []struct {
		id   string
		owns bool
	}{
		{"mcp__playwright__click", true},
		{"mcp__playwright__missing", true},
		{"mcp__playwright2__click", false},
		{"mcp__other__click", false},
		{"read", false},
	} {
		t.Run(tt.id, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.owns, frames.Owns(tt.id))
		})
	}
}
