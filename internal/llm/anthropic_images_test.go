package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

// newVisionTestClient builds a client whose catalog declares (or with vision=false,
// omits) image input, plus a real 1x1 PNG on disk.
func newVisionTestClient(t *testing.T, vision bool) (*anthropicClient, string) {
	t.Helper()

	pngPath := filepath.Join(t.TempDir(), "coagent-test.png")
	png := []byte{
		0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', // real magic bytes are irrelevant to the driver
	}
	require.NoError(t, os.WriteFile(pngPath, png, 0o600))

	var modalities []string
	if vision {
		modalities = []string{"text", "image"}
	}

	c, err := newAnthropicClient(anthropicParams{
		APIKey: "key",
		Model: config.ModelEntry{
			ID:              "claude-opus-5",
			MaxTokens:       4096,
			ContextWindow:   200000,
			InputModalities: modalities,
		},
	})
	require.NoError(t, err)

	return c.(*anthropicClient), pngPath
}

func messageParamBody(t *testing.T, msg anthropic.MessageParam) map[string]any {
	t.Helper()

	raw, err := json.Marshal(msg)
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))

	return body
}

func blockMap(t *testing.T, content any, idx int) map[string]any {
	t.Helper()

	blocks, ok := content.([]any)
	require.True(t, ok)
	require.Greater(t, len(blocks), idx)

	block, ok := blocks[idx].(map[string]any)
	require.True(t, ok)

	return block
}

func innerBlocks(t *testing.T, outer map[string]any) []any {
	t.Helper()

	inner, ok := outer["content"].([]any)
	if !ok {
		return nil
	}

	return inner
}

func TestAnthropic_ImageBearingToolResult(t *testing.T) {
	c, png := newVisionTestClient(t, true)
	msg := llmwire.Message{
		Role:       llmwire.RoleTool,
		Content:    "[/tmp/coagent-x.png]\nimage loaded",
		ToolCallID: "call-img",
		Images: []llmwire.ImageRef{
			{Path: png, Mime: llmwire.MimeImagePng, Size: int64(len("[/tmp/coagent-x.png]"))},
		},
	}

	body := messageParamBody(t, c.buildToolResultMessage(msg))
	outer := blockMap(t, body["content"], 0)
	assert.Equal(t, "call-img", outer["tool_use_id"])
	assert.Equal(t, false, outer["is_error"])

	blocks := innerBlocks(t, outer)
	require.Len(t, blocks, 2, "text first, then the image slot")

	text, ok := blocks[0].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, text["text"], "/tmp/coagent-x.png")

	image, ok := blocks[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "image", image["type"])

	source, ok := image["source"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "base64", source["type"])
	assert.Equal(t, "image/png", source["media_type"])
	assert.NotEmpty(t, source["data"], "pixels must be materialized at request build")
}

func TestAnthropic_UnreadableImageDegradesToPlaceholder(t *testing.T) {
	c, _ := newVisionTestClient(t, true)
	msg := llmwire.Message{
		Role:       llmwire.RoleTool,
		Content:    "[gone]",
		ToolCallID: "call-gone",
		ToolName:   "read",
		Images:     []llmwire.ImageRef{{Path: "/nonexistent/deleted.png", Mime: llmwire.MimeImagePng, Size: 12}},
	}

	body := messageParamBody(t, c.buildToolResultMessage(msg))
	blocks := innerBlocks(t, blockMap(t, body["content"], 0))
	require.Len(t, blocks, 2, "placeholder must occupy the degraded slot")

	placeholder, ok := blocks[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, llmwire.ImagePlaceholder(llmwire.ImageOmitReasonUnreadable), placeholder["text"])
	assert.Nil(t, blocks[1].(map[string]any)["source"])
}

func TestAnthropic_NonVisionModelNeverSendsPixels(t *testing.T) {
	c, png := newVisionTestClient(t, false)
	msg := llmwire.Message{
		Role:       llmwire.RoleTool,
		Content:    "[img]",
		ToolCallID: "call-text-only",
		ToolName:   "read",
		Images: []llmwire.ImageRef{
			{Path: png, Mime: llmwire.MimeImagePng, Size: 10},
			{Path: png, Mime: llmwire.MimeImageGif, Size: 10},
		},
	}

	body := messageParamBody(t, c.buildToolResultMessage(msg))
	blocks := innerBlocks(t, blockMap(t, body["content"], 0))
	require.Len(t, blocks, 3, "text + one degraded slot per ref")

	for _, b := range blocks {
		block := b.(map[string]any)
		assert.Equal(t, "text", block["type"], "a non-vision model receives only text")
	}

	ph1 := blocks[1].(map[string]any)
	assert.Equal(t, llmwire.ImagePlaceholder(llmwire.ImageOmitReasonNoVision), ph1["text"])
	ph2 := blocks[2].(map[string]any)
	assert.Equal(t, llmwire.ImagePlaceholder(llmwire.ImageOmitReasonNoVision), ph2["text"])
}

func TestAnthropic_UnsupportedMimeDegradesToPlaceholder(t *testing.T) {
	c, _ := newVisionTestClient(t, true)
	msg := llmwire.Message{
		Role:       llmwire.RoleTool,
		Content:    "[pdf]",
		ToolCallID: "call-pdf",
		ToolName:   "read",
		Images:     []llmwire.ImageRef{{Path: "/tmp/x.pdf", Mime: "application/pdf", Size: 5}},
	}

	body := messageParamBody(t, c.buildToolResultMessage(msg))
	blocks := innerBlocks(t, blockMap(t, body["content"], 0))
	require.Len(t, blocks, 2)

	ph := blocks[1].(map[string]any)
	assert.Equal(t, llmwire.ImagePlaceholder(llmwire.ImageOmitReasonUnsupported), ph["text"])
}

func TestAnthropic_CacheBreakpointSurvivesImageToolResult(t *testing.T) {
	c, png := newVisionTestClient(t, true)

	msgs := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "task"},
		{
			Role:      llmwire.RoleAssistant,
			ToolCalls: []llmwire.ToolCall{{ID: "call-cache", Name: "read", Arguments: []byte(`{}`)}},
		},
		{
			Role:       llmwire.RoleTool,
			Content:    "[img]",
			ToolCallID: "call-cache",
			ToolName:   "read",
			Images:     []llmwire.ImageRef{{Path: png, Mime: llmwire.MimeImagePng, Size: 10}},
		},
	}

	params := c.buildMessageParams("system", msgs, nil, 1024)

	// The breakpoint must remain ON the outer tool_result block — nested image
	// blocks inside its content array must not move or shadow it.
	result := params.Messages[len(params.Messages)-1].Content[0]
	require.NotNil(t, result.OfToolResult, "image-bearing result stays a tool_result block")
	assert.NotNil(t, result.OfToolResult.CacheControl, "sliding-window breakpoint intact")
	require.Len(t, result.OfToolResult.Content, 2, "nested text+image survive alongside the marker")
}
