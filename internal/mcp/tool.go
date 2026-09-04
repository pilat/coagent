package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pilat/coagent/internal/tool"
)

// mcpTool is a direct MCP tool: metadata comes from the activation's immutable
// snapshot and the live client is resolved at execution time, so the tool
// inventory and the provider schemas always originate from the same snapshot.
type mcpTool struct {
	serverName string
	meta       ToolMeta
	clientFor  func(ctx context.Context) (*Client, error)
}

// newMCPTool builds a direct MCP tool from immutable metadata; clientFor
// supplies the live client at execution time.
func newMCPTool(
	serverName string,
	meta ToolMeta,
	clientFor func(ctx context.Context) (*Client, error),
) *mcpTool {
	return &mcpTool{
		serverName: serverName,
		meta:       meta,
		clientFor:  clientFor,
	}
}

// newLiveMCPTool pins a live client: metadata is projected from the client's
// discovered tools and every execution runs on that client.
func newLiveMCPTool(serverName, toolName string, client *Client) *mcpTool {
	return newMCPTool(serverName, toolMetaOf(client, toolName), func(context.Context) (*Client, error) {
		return client, nil
	})
}

func toolMetaOf(client *Client, toolName string) ToolMeta {
	mcpTool, ok := client.tools[toolName]
	if !ok {
		return ToolMeta{Name: toolName}
	}

	schema, err := client.ToolSchema(toolName)
	if err != nil || len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}

	return ToolMeta{
		Name:        toolName,
		Description: mcpTool.Description,
		Schema:      append(json.RawMessage(nil), schema...),
	}
}

func (t *mcpTool) ID() string {
	return fmt.Sprintf("mcp__%s__%s", t.serverName, t.meta.Name)
}

// Remote concurrency annotations are hints, not a trusted contract.
func (t *mcpTool) ParallelSafe() bool { return false }

func (t *mcpTool) Description() string {
	return t.meta.Description
}

func (t *mcpTool) Parameters() json.RawMessage {
	if len(t.meta.Schema) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}

	return t.meta.Schema
}

func (t *mcpTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var args map[string]any
	if err := json.Unmarshal(params, &args); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	client, err := t.clientFor(ctx)
	if err != nil {
		return nil, err
	}

	output, err := client.CallTool(ctx, t.meta.Name, args)
	if err != nil {
		return nil, err
	}

	return &tool.Result{
		Title:  fmt.Sprintf("MCP: %s/%s", t.serverName, t.meta.Name),
		Output: output,
		Metadata: map[string]any{
			"server": t.serverName,
			"tool":   t.meta.Name,
		},
	}, nil
}
