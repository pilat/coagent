package hooks

import "encoding/json"

type stdinPayload interface {
	hookPayload()
}

type commonPayload struct {
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
}

type toolPayload struct {
	commonPayload
	ToolName        string          `json:"tool_name"`
	CoagentToolName string          `json:"coagent_tool_name"`
	ToolInput       json.RawMessage `json:"tool_input"`
	ToolUseID       string          `json:"tool_use_id"`
}

type postToolPayload struct {
	toolPayload
	ToolResponse any `json:"tool_response"`
}

type stopPayload struct {
	commonPayload
	StopHookActive       bool   `json:"stop_hook_active"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

type defaultToolResponse struct {
	Output string `json:"output"`
}

func (toolPayload) hookPayload() {}

func (stopPayload) hookPayload() {}
