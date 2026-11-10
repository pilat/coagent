package builtin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBrowserFrameRule(t *testing.T) {
	t.Parallel()

	frames := (&Stack{frames: browserFrameRule{serverName: "remote"}}).Frames()
	require.NotNil(t, frames)
	assert.Equal(t, 120000, frames.ResultLimit())

	for _, tt := range []struct {
		id   string
		owns bool
	}{
		{"mcp__remote__click", true},
		{"mcp__remote__missing", true},
		{"mcp__remote2__click", false},
		{"mcp__playwright__click", false},
		{"mcp__other__click", false},
		{"read", false},
	} {
		t.Run(tt.id, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.owns, frames.Owns(tt.id))
		})
	}
}

func TestBrowserFrameRuleWithoutBinding(t *testing.T) {
	t.Parallel()
	for _, stack := range []*Stack{nil, {}} {
		frames := stack.Frames()
		require.NotNil(t, frames)
		assert.Equal(t, 120000, frames.ResultLimit())
		for _, id := range []string{"mcp__playwright__click", "mcp__remote__click", "read", "mcp____click"} {
			assert.False(t, frames.Owns(id))
		}
	}
}
