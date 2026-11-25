package builtin

import "github.com/pilat/coagent/internal/tool"

var _ tool.HookResponder = (*bashTool)(nil)

type bashHookResponse struct {
	Stdout      string `json:"stdout"`
	Stderr      string `json:"stderr"`
	Interrupted bool   `json:"interrupted"`
	IsImage     bool   `json:"isImage"`
}

func (t *bashTool) HookResponse(result *tool.Result) any {
	return bashHookResponse{
		Stdout:      result.Output,
		Stderr:      "",
		Interrupted: false,
		IsImage:     false,
	}
}
