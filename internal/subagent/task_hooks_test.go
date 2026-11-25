package subagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

func TestTaskHookName(t *testing.T) {
	candidate := taskToolWith(&mockSpawner{})
	named, ok := candidate.(tool.HookNamed)
	require.True(t, ok)
	assert.Equal(t, tool.HookName{
		Name: "Agent", Aliases: []string{"Task", "spawn_agent", "subagent"},
	}, named.HookName())
}
