package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

type fixedAttachmentSink struct{ note string }

func (s fixedAttachmentSink) Store(BinaryPart) (Attachment, error) {
	return Attachment{Note: s.note}, nil
}

type recordingCallClient struct {
	mcpclient.MCPClient
	result *mcp.CallToolResult
	calls  []mcp.CallToolRequest
}

func (c *recordingCallClient) CallTool(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c.calls = append(c.calls, request)
	return c.result, nil
}

func TestMCPBinaryContentNeverBecomesText(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("binary-secret"))
	client := &Client{client: &recordingCallClient{result: &mcp.CallToolResult{Content: []mcp.Content{
		mcp.TextContent{Text: "visible"},
		mcp.ImageContent{MIMEType: "image/png", Data: encoded},
		&mcp.AudioContent{MIMEType: "audio/wav", Data: encoded},
		mcp.EmbeddedResource{Resource: mcp.BlobResourceContents{MIMEType: "application/octet-stream", Blob: encoded}},
		&mcp.EmbeddedResource{Resource: &mcp.TextResourceContents{Text: "resource text"}},
	}}}}
	got, err := client.CallTool(t.Context(), "x", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"visible", "resource text"}, got.Text)
	require.Len(t, got.Binary, 3)
	for _, part := range got.Binary {
		assert.Equal(t, []byte("binary-secret"), part.Data)
	}
	for _, text := range got.Text {
		assert.NotContains(t, text, encoded)
	}

	client.client = &recordingCallClient{result: &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
		mcp.TextContent{Text: "server failed"}, &mcp.ImageContent{MIMEType: "image/png", Data: encoded},
	}}}
	failed, err := client.CallTool(t.Context(), "x", nil)
	require.NoError(t, err)
	assert.True(t, failed.IsError)
	assert.Empty(t, failed.Binary)
	assert.Contains(t, failed.Text, "server failed")
	assert.NotContains(t, failed.Text, encoded)
	toolResult, err := newLiveMCPTool("other", "x", client, nil).Execute(t.Context(), json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.True(t, toolResult.IsError)
	assert.True(t, toolResult.Untrusted)
	assert.NotContains(t, toolResult.Output, encoded)
}

func TestPlaywrightTaskStateValidationAndForwarding(t *testing.T) {
	remote := &recordingCallClient{result: &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Text: "frame"}}}}
	client := &Client{
		client: remote,
		tools:  map[string]mcp.Tool{"click": {Name: "click", InputSchema: mcp.ToolInputSchema{Type: "object"}}},
	}
	browser := newLiveMCPTool(tool.PlaywrightServerName, "click", client, nil)
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	require.NoError(t, json.Unmarshal(browser.Parameters(), &schema))
	assert.Contains(t, schema.Properties, tool.BrowserTaskState)
	assert.Contains(t, schema.Required, tool.BrowserTaskState)
	for _, args := range []string{`{}`, `{"task_state":"  "}`, `{"task_state":42}`, `broken`} {
		result, err := browser.Execute(t.Context(), json.RawMessage(args))
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.False(t, result.Untrusted)
	}
	assert.Empty(t, remote.calls)
	result, err := browser.Execute(t.Context(), json.RawMessage(`{"task_state":"continue","x":1}`))
	require.NoError(t, err)
	assert.False(t, result.IsError)
	require.Len(t, remote.calls, 1)
	assert.Equal(t, map[string]any{"x": float64(1)}, remote.calls[0].Params.Arguments)
	assert.True(t, result.Untrusted)

	other := newLiveMCPTool("playwright2", "click", client, nil)
	assert.JSONEq(t, string(toolMetaOf(client, "click").Schema), string(other.Parameters()))
}

func TestRegisterToolsUsesActivationSinkAndSkipsTaskStateCollision(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("blob"))
	remote := &recordingCallClient{result: &mcp.CallToolResult{Content: []mcp.Content{
		mcp.ImageContent{MIMEType: "image/png", Data: encoded},
	}}}
	client := &Client{client: remote, tools: map[string]mcp.Tool{
		"safe": {Name: "safe", InputSchema: mcp.ToolInputSchema{Type: "object"}},
		"collision": {
			Name: "collision",
			InputSchema: mcp.ToolInputSchema{
				Type:       "object",
				Properties: map[string]any{tool.BrowserTaskState: map[string]any{"type": "string"}},
			},
		},
	}}
	service := &svc{clients: map[string]*Client{tool.PlaywrightServerName: client}}
	for _, note := range []string{"first activation", "second activation"} {
		registry := tool.NewRegistry()
		assert.Equal(t, 1, service.RegisterTools(registry, fixedAttachmentSink{note: note}))
		assert.Nil(t, registry.Get(tool.PlaywrightToolPrefix+"collision"))
		result, err := registry.Get(tool.PlaywrightToolPrefix+"safe").
			Execute(t.Context(), json.RawMessage(`{"task_state":"continue"}`))
		require.NoError(t, err)
		assert.Contains(t, result.Output, note)
	}
}
