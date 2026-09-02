package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pilat/coagent/internal/tool"
)

// mcpTool wraps an MCP tool as a coagent Tool.
type mcpTool struct {
	serverName string
	toolName   string
	client     *Client
}

func newMCPTool(serverName, toolName string, client *Client) *mcpTool {
	return &mcpTool{
		serverName: serverName,
		toolName:   toolName,
		client:     client,
	}
}

func (t *mcpTool) ID() string {
	return fmt.Sprintf("mcp__%s__%s", t.serverName, t.toolName)
}

// Remote concurrency annotations are hints, not a trusted contract.
func (t *mcpTool) ParallelSafe() bool { return false }

func (t *mcpTool) Description() string {
	if t.client == nil {
		return ""
	}

	mcpTool, ok := t.client.tools[t.toolName]
	if !ok {
		return ""
	}

	return mcpTool.Description
}

func (t *mcpTool) Parameters() json.RawMessage {
	schema, err := t.client.ToolSchema(t.toolName)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}

	return schema
}

func (t *mcpTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var args map[string]any
	if err := json.Unmarshal(params, &args); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	output, err := t.client.CallTool(ctx, t.toolName, args)
	if err != nil {
		return nil, err
	}

	return &tool.Result{
		Title:  fmt.Sprintf("MCP: %s/%s", t.serverName, t.toolName),
		Output: output,
		Metadata: map[string]any{
			"server": t.serverName,
			"tool":   t.toolName,
		},
	}, nil
}
