package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

type fixedAttachmentSink struct{ note string }

type testProfile struct{}

func (testProfile) Admit(meta ToolMeta) bool { return meta.Name == "safe" }

func (testProfile) Schema(input json.RawMessage) json.RawMessage {
	var schema map[string]any
	if err := json.Unmarshal(input, &schema); err != nil {
		panic(err)
	}
	schema["profile"] = true
	encoded, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	return encoded
}

func (testProfile) Gate(args map[string]any, decodeErr error) *tool.Result {
	value, ok := args["gate"].(string)
	if decodeErr != nil || !ok || strings.TrimSpace(value) == "" {
		return &tool.Result{IsError: true, Output: "gate required"}
	}
	delete(args, "gate")
	return nil
}

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
	toolResult, err := newLiveMCPTool("other", "x", client, nil, nil).Execute(t.Context(), json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.True(t, toolResult.IsError)
	assert.True(t, toolResult.Untrusted)
	assert.NotContains(t, toolResult.Output, encoded)
}

func TestProfileGatesArgumentsBeforeForwarding(t *testing.T) {
	remote := &recordingCallClient{result: &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Text: "frame"}}}}
	client := &Client{
		client: remote,
		tools:  map[string]mcp.Tool{"click": {Name: "click", InputSchema: mcp.ToolInputSchema{Type: "object"}}},
	}
	adapted := newLiveMCPTool("example", "click", client, nil, testProfile{})
	assert.JSONEq(t, `{"type":"object","properties":{},"required":[],"profile":true}`, string(adapted.Parameters()))
	for _, args := range []string{`{}`, `{"gate":"  "}`, `{"gate":42}`, `broken`} {
		result, err := adapted.Execute(t.Context(), json.RawMessage(args))
		require.NoError(t, err)
		assert.Equal(t, &tool.Result{IsError: true, Output: "gate required"}, result)
	}
	assert.Empty(t, remote.calls)
	result, err := adapted.Execute(t.Context(), json.RawMessage(`{"gate":"continue","x":1}`))
	require.NoError(t, err)
	assert.False(t, result.IsError)
	require.Len(t, remote.calls, 1)
	assert.Equal(t, map[string]any{"x": float64(1)}, remote.calls[0].Params.Arguments)
	assert.True(t, result.Untrusted)

	other := newLiveMCPTool("playwright", "click", client, nil, nil)
	assert.Equal(t, toolMetaOf(client, "click").Schema, other.Parameters())
	_, err = other.Execute(t.Context(), json.RawMessage(`broken`))
	require.ErrorContains(t, err, "invalid parameters:")
	assert.Len(t, remote.calls, 1)
}

func TestRegisterToolsUsesActivationSinkAndProfileAdmission(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("blob"))
	remote := &recordingCallClient{result: &mcp.CallToolResult{Content: []mcp.Content{
		mcp.ImageContent{MIMEType: "image/png", Data: encoded},
	}}}
	client := &Client{client: remote, tools: map[string]mcp.Tool{
		"safe":     {Name: "safe", InputSchema: mcp.ToolInputSchema{Type: "object"}},
		"rejected": {Name: "rejected", InputSchema: mcp.ToolInputSchema{Type: "object"}},
	}}
	service := &svc{clients: map[string]*Client{"example": client}}
	for _, note := range []string{"first activation", "second activation"} {
		registry := tool.NewRegistry()
		assert.Equal(
			t,
			1,
			service.RegisterTools(
				registry,
				fixedAttachmentSink{note: note},
				map[string]Profile{"example": testProfile{}},
			),
		)
		assert.Nil(t, registry.Get("mcp__example__rejected"))
		adapted := registry.Get("mcp__example__safe")
		require.NotNil(t, adapted)
		assert.Contains(t, string(adapted.Parameters()), `"profile":true`)
		result, err := adapted.Execute(t.Context(), json.RawMessage(`{"gate":"continue"}`))
		require.NoError(t, err)
		assert.Contains(t, result.Output, note)
	}
}
