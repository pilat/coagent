package builtin

import (
	"encoding/json"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/tool"
)

const (
	browserFrameResultLimit = 120000
	browserTaskStateArg     = "task_state"
)

var (
	_ tool.FrameRule = browserFrameRule{}
	_ mcp.Profile    = browserProfile{}
)

type browserFrameRule struct{ serverName string }

type browserProfile struct{}

// Frames returns the stack's frame rule independently of which servers are up.
func (s *Stack) Frames() tool.FrameRule {
	if s == nil {
		return browserFrameRule{}
	}

	return s.frames
}

func (r browserFrameRule) Owns(id string) bool {
	return r.serverName != "" && strings.HasPrefix(id, "mcp__"+r.serverName+"__")
}

func (browserFrameRule) ResultLimit() int {
	return browserFrameResultLimit
}

func (browserProfile) Admit(meta mcp.ToolMeta) bool {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(meta.Schema, &schema) == nil && schema.Properties != nil {
		if _, collision := schema.Properties[browserTaskStateArg]; collision {
			logger.Named("tool.browser").Warn("browser_task_state_collision", zap.String("tool", meta.Name))
			return false
		}
	}

	return true
}

func (browserProfile) Schema(input json.RawMessage) json.RawMessage {
	var schema map[string]any
	if err := json.Unmarshal(input, &schema); err != nil || schema == nil {
		schema = map[string]any{"type": "object"}
	}

	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		properties = make(map[string]any)
	}

	properties[browserTaskStateArg] = map[string]any{
		"type":        "string",
		"description": "In up to five sentences, say whether the previous action succeeded, failed, or is uncertain based on the current frame; preserve task facts from this frame (names, prices, URLs, counts, exclusions); and name the next immediate goal. Do not describe screen coordinates or layout.",
	}
	schema["properties"] = properties
	required, _ := schema["required"].([]any)
	schema["required"] = append(required, browserTaskStateArg)

	encoded, err := json.Marshal(schema)
	if err != nil {
		return input
	}

	return encoded
}

func (browserProfile) Gate(args map[string]any, decodeErr error) *tool.Result {
	state, ok := args[browserTaskStateArg].(string)
	if decodeErr != nil || !ok || strings.TrimSpace(state) == "" {
		return &tool.Result{
			IsError: true,
			Output:  "Every surface call requires a non-blank string task_state recording the previous action's outcome, task facts from the current frame, and the next goal.",
		}
	}

	delete(args, browserTaskStateArg)

	return nil
}
