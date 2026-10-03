package sessionprompt

import (
	"context"
	"encoding/json"

	"github.com/pilat/coagent/internal/tool"
)

type stubTool struct{ id string }

func (t *stubTool) ID() string                { return t.id }
func (*stubTool) Description() string         { return "prompt fixture" }
func (*stubTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (*stubTool) ParallelSafe() bool          { return false }
func (*stubTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	panic("prompt inventory must not execute tools")
}
