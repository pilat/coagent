package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

func TestFilterRegistryForAgent_BuiltInSubagentExcludesTodoTools(t *testing.T) {
	set := registry.NewSet(nil)

	cfg, ok := set.Get(registry.AgentTypeGeneral)
	require.True(t, ok)

	filtered := filterRegistryForAgent(set, testRegistry(), cfg)

	assert.Nil(t, filtered.Get("todoread"))
	assert.Nil(t, filtered.Get("todowrite"))
	assert.NotNil(t, filtered.Get("read"))
}

func TestFilterRegistryForAgent_DynamicSubagentExcludesTodoTools(t *testing.T) {
	customType := registry.AgentType("custom-subagent-dynamic-star")
	set := registry.NewSet([]registry.AgentTypeConfig{{
		Name:  customType,
		Mode:  registry.ModeSubagent,
		Tools: []string{"*"},
	}})

	cfg, ok := set.Get(customType)
	require.True(t, ok)

	filtered := filterRegistryForAgent(set, testRegistry(), cfg)

	assert.Nil(t, filtered.Get("todoread"))
	assert.Nil(t, filtered.Get("todowrite"))
	assert.NotNil(t, filtered.Get("read"))
}

func TestFilterRegistryForAgent_BuildAgentKeepsTodoTools(t *testing.T) {
	set := registry.NewSet(nil)

	cfg, ok := set.Get(registry.AgentTypeBuild)
	require.True(t, ok)

	filtered := filterRegistryForAgent(set, testRegistry(), cfg)

	assert.NotNil(t, filtered.Get("todoread"))
	assert.NotNil(t, filtered.Get("todowrite"))
	assert.NotNil(t, filtered.Get("read"))
}

// batch dispatches through a registry of its own, so the allowlist only holds
// if that registry is the filtered one.
func TestFilterRegistryForAgent_BatchCannotReachExcludedTools(t *testing.T) {
	customType := registry.AgentType("custom-subagent-batch")
	set := registry.NewSet([]registry.AgentTypeConfig{{
		Name:  customType,
		Mode:  registry.ModeSubagent,
		Tools: []string{"read", tool.IDBatch},
	}})

	cfg, ok := set.Get(customType)
	require.True(t, ok)

	full := testRegistry()
	full.Register(builtin.NewBatchTool(full))

	filtered := filterRegistryForAgent(set, full, cfg)
	require.NotNil(t, filtered.Get(tool.IDBatch))
	require.Nil(t, filtered.Get("bash"))

	_, err := filtered.Execute(
		context.Background(),
		tool.IDBatch,
		json.RawMessage(`{"calls":[{"tool":"bash","params":{}}]}`),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown tool "bash"`)
}

func testRegistry() tool.Registry {
	reg := tool.NewRegistry()
	reg.Register(testTool{id: "read"})
	reg.Register(testTool{id: "bash"})
	reg.Register(testTool{id: "todoread"})
	reg.Register(testTool{id: "todowrite"})

	return reg
}

type testTool struct {
	id string
}

func (t testTool) ID() string                  { return t.id }
func (t testTool) Description() string         { return "test tool" }
func (t testTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (t testTool) ParallelSafe() bool          { return false }
func (t testTool) Execute(_ context.Context, _ json.RawMessage) (*tool.Result, error) {
	return &tool.Result{Output: "ok"}, nil
}
