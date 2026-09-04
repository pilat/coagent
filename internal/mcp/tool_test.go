package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/pilat/coagent/internal/tool"
)

// mockMCPClient is a mock implementation for testing
type mockMCPClient struct {
	tools map[string]mcp.Tool
}

func newMockMCPClient() *mockMCPClient {
	return &mockMCPClient{
		tools: map[string]mcp.Tool{
			"test_tool": {
				Name:        "test_tool",
				Description: "A test tool",
				InputSchema: mcp.ToolInputSchema{
					Type: "object",
					Properties: map[string]any{
						"param1": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
}

func (m *mockMCPClient) Tools() map[string]mcp.Tool {
	return m.tools
}

func (m *mockMCPClient) ToolSchema(name string) (json.RawMessage, error) {
	t, ok := m.tools[name]
	if !ok {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	schema, _ := json.Marshal(t.InputSchema)
	return schema, nil
}

func (m *mockMCPClient) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	return `{"result": "success"}`, nil
}

func TestMCPTool_ID(t *testing.T) {
	client := newMockMCPClient()
	mcpTool := newLiveMCPTool("test_server", "test_tool", &Client{
		name:  "test_server",
		tools: client.tools,
	})

	expected := "mcp__test_server__test_tool"
	if got := mcpTool.ID(); got != expected {
		t.Errorf("ID() = %q, want %q", got, expected)
	}
}

func TestMCPTool_ID_SpecialCharacters(t *testing.T) {
	tests := []struct {
		serverName string
		toolName   string
		expected   string
	}{
		{"my-server", "my-tool", "mcp__my-server__my-tool"},
		{"server_123", "tool_456", "mcp__server_123__tool_456"},
		{"gopls", "workspace/symbol", "mcp__gopls__workspace/symbol"},
	}

	for _, tt := range tests {
		t.Run(tt.serverName+"_"+tt.toolName, func(t *testing.T) {
			client := &Client{name: tt.serverName, tools: map[string]mcp.Tool{}}
			mcpTool := newLiveMCPTool(tt.serverName, tt.toolName, client)

			if got := mcpTool.ID(); got != tt.expected {
				t.Errorf("ID() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestMCPTool_Description(t *testing.T) {
	client := newMockMCPClient()
	wrappedClient := &Client{
		name:  "test_server",
		tools: client.tools,
	}
	mcpTool := newLiveMCPTool("test_server", "test_tool", wrappedClient)

	expected := "A test tool"
	if got := mcpTool.Description(); got != expected {
		t.Errorf("Description() = %q, want %q", got, expected)
	}
}

func TestMCPTool_Description_NotFound(t *testing.T) {
	client := &Client{
		name:  "test_server",
		tools: map[string]mcp.Tool{},
	}
	mcpTool := newLiveMCPTool("test_server", "nonexistent", client)

	// Should return empty string for a tool the client never discovered
	if got := mcpTool.Description(); got != "" {
		t.Errorf("Description() = %q, want empty string", got)
	}
}

func TestMCPTool_Parameters(t *testing.T) {
	client := newMockMCPClient()
	wrappedClient := &Client{
		name:  "test_server",
		tools: client.tools,
	}
	mcpTool := newLiveMCPTool("test_server", "test_tool", wrappedClient)

	params := mcpTool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Errorf("Parameters() returned invalid JSON: %v", err)
	}

	if schema["type"] != "object" {
		t.Errorf("Parameters() schema type = %v, want 'object'", schema["type"])
	}
}

func TestMCPTool_ResultStructure(t *testing.T) {
	result := &tool.Result{
		Title:  "MCP: test_server/test_tool",
		Output: `{"result": "success"}`,
		Metadata: map[string]any{
			"server": "test_server",
			"tool":   "test_tool",
		},
	}

	if result.Title != "MCP: test_server/test_tool" {
		t.Errorf("Title = %q, want %q", result.Title, "MCP: test_server/test_tool")
	}

	if result.Metadata["server"] != "test_server" {
		t.Errorf("Metadata['server'] = %v, want 'test_server'", result.Metadata["server"])
	}

	if result.Metadata["tool"] != "test_tool" {
		t.Errorf("Metadata['tool'] = %v, want 'test_tool'", result.Metadata["tool"])
	}
}
