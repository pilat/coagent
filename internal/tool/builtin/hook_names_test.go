package builtin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

func TestHookNamesDeclared(t *testing.T) {
	tests := []struct {
		candidate tool.Tool
		want      tool.HookName
	}{
		{candidate: &bashTool{}, want: tool.HookName{Name: "Bash", Aliases: []string{"shell"}}},
		{candidate: &readTool{}, want: tool.HookName{Name: "Read"}},
		{candidate: &writeTool{}, want: tool.HookName{Name: "Write"}},
		{candidate: &editTool{}, want: tool.HookName{Name: "Edit"}},
		{candidate: &grepTool{}, want: tool.HookName{Name: "Grep"}},
		{candidate: &globTool{}, want: tool.HookName{Name: "Glob"}},
		{candidate: &webFetchTool{}, want: tool.HookName{Name: "WebFetch"}},
		{candidate: &webSearchTool{}, want: tool.HookName{Name: "WebSearch"}},
	}
	for _, tt := range tests {
		t.Run(tt.candidate.ID(), func(t *testing.T) {
			named, ok := tt.candidate.(tool.HookNamed)
			require.True(t, ok)
			assert.Equal(t, tt.want, named.HookName())
		})
	}
}

func TestHookNamesNeverShadowAnotherToolID(t *testing.T) {
	stack, err := BuildStack(context.Background(), StackConfig{
		WorkDir: t.TempDir(), Loader: loader.New(), Todo: todo.New(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = stack.Close() })
	for _, candidate := range stack.Registry.List() {
		named, ok := candidate.(tool.HookNamed)
		if !ok {
			continue
		}
		identity := named.HookName()
		for _, name := range append([]string{identity.Name}, identity.Aliases...) {
			if other := stack.Registry.Get(name); other != nil {
				assert.Equal(t, candidate.ID(), other.ID(), "hook name %s shadows a native ID", name)
			}
		}
	}
}
