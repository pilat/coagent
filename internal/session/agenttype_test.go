package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

func TestNewWithOptions_AgentTypeMaxIterationsAndTools(t *testing.T) {
	tests := []struct {
		name        string
		agentType   registry.AgentType
		wantMaxIter int
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:        "general subagent: 25 iterations, no todo tools",
			agentType:   registry.AgentTypeGeneral,
			wantMaxIter: 25,
			wantPresent: []string{"read", "write", "edit", "bash"},
			wantAbsent:  []string{"todoread", "todowrite"},
		},
		{
			name:        "explore subagent: 10 iterations, read-only set",
			agentType:   registry.AgentTypeExplore,
			wantMaxIter: 10,
			wantPresent: []string{"read", "grep", "glob", "ls", "bash"},
			wantAbsent:  []string{"write", "edit", "todoread", "todowrite"},
		},
		{
			name:        "build primary: unlimited, keeps todo tools",
			agentType:   registry.AgentTypeBuild,
			wantMaxIter: 0,
			wantPresent: []string{"read", "write", "edit", "bash", "todoread", "todowrite"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := tool.NewRegistry()
			for _, id := range []string{"read", "write", "edit", "grep", "glob", "ls", "bash", "todoread", "todowrite"} {
				reg.Register(testTool{id: id})
			}

			p := params{
				Config:    &config.Config{WorkDir: t.TempDir(), Model: "test-model"},
				LLMClient: &mockLLMClient{},
				TodoStore: todo.New(),
				Loader:    loader.New(),
				Registry:  reg,
			}

			sess, err := newWithOptions(context.Background(), p, options{ID: 1, AgentType: tc.agentType})
			require.NoError(t, err)

			s := sess.(*svc)
			assert.Equal(t, tc.wantMaxIter, s.maxIterations)

			for _, id := range tc.wantPresent {
				assert.NotNil(t, s.registry.Get(id), "expected tool %q present", id)
			}

			for _, id := range tc.wantAbsent {
				assert.Nil(t, s.registry.Get(id), "expected tool %q absent", id)
			}
		})
	}
}
