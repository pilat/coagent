package builtin

import "github.com/pilat/coagent/internal/tool"

var _ tool.HookResponder = (*bashTool)(nil)

// A map, not a struct: Claude Code's `isImage` key is camelCase and local JSON tags are snake_case.
func (t *bashTool) HookResponse(result *tool.Result) any {
	return map[string]any{
		"stdout":      result.Output,
		"stderr":      "",
		"interrupted": false,
		"isImage":     false,
	}
}
