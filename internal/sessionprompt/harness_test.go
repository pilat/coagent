package sessionprompt

import (
	"context"
	"encoding/json"

	"github.com/pilat/coagent/internal/tool"
)

// Mirrors the tools' own declarations; sessionbuild's byte goldens pin the real ones.
var stubDeclarations = map[string]tool.PromptDeclaration{
	"read":        {Group: tool.PromptGroupFiles, Order: 1, Description: "view file"},
	"write":       {Group: tool.PromptGroupFiles, Order: 2, Description: "create/overwrite"},
	"edit":        {Group: tool.PromptGroupFiles, Order: 3, Description: "find-replace"},
	"apply_patch": {Group: tool.PromptGroupFiles, Order: 4, Description: "unified diff"},
	"glob":        {Group: tool.PromptGroupFiles, Order: 5, Description: "find by pattern"},
	"grep":        {Group: tool.PromptGroupFiles, Order: 6, Description: "search contents"},
	"ls":          {Group: tool.PromptGroupFiles, Order: 7, Description: "list directory"},
	"bash":        {Group: tool.PromptGroupShell, Order: 1},
	"lsp": {
		Group:       tool.PromptGroupCodeIntelligence,
		Order:       1,
		Description: "definitions, references, types, call hierarchy",
	},
	"todoread":      {Group: tool.PromptGroupTaskTracking, Order: 1},
	"todowrite":     {Group: tool.PromptGroupTaskTracking, Order: 2},
	"memory_save":   {Group: tool.PromptGroupMemory, Order: 1},
	"memory_delete": {Group: tool.PromptGroupMemory, Order: 2},
	"skill":         {Group: tool.PromptGroupSkills, Order: 1, Description: "load domain knowledge"},
	"webfetch":      {Group: tool.PromptGroupWeb, Order: 1, Description: "fetch known URL"},
	"websearch":     {Group: tool.PromptGroupWeb, Order: 2, Description: "web search"},
}

type stubTool struct{ id string }

func (t *stubTool) ID() string                                { return t.id }
func (*stubTool) Description() string                         { return "prompt fixture" }
func (t *stubTool) PromptDeclaration() tool.PromptDeclaration { return stubDeclarations[t.id] }
func (*stubTool) Parameters() json.RawMessage                 { return json.RawMessage(`{}`) }
func (*stubTool) ParallelSafe() bool                          { return false }
func (*stubTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	panic("prompt inventory must not execute tools")
}
