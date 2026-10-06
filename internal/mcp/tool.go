package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

type Attachment struct {
	Note  string
	Image *llmwire.ImageRef
}

type AttachmentSink interface {
	Store(BinaryPart) (Attachment, error)
}

// ToolMeta is the model-facing projection of one discovered MCP tool.
type ToolMeta struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// mcpTool keeps the metadata discovered for one stack and calls its live client.
type mcpTool struct {
	serverName string
	meta       ToolMeta
	clientFor  func(ctx context.Context) (*Client, error)
	sink       AttachmentSink
}

// newMCPTool builds a direct MCP tool from discovered metadata.
func newMCPTool(
	serverName string,
	meta ToolMeta,
	clientFor func(ctx context.Context) (*Client, error),
	sink AttachmentSink,
) *mcpTool {
	return &mcpTool{
		serverName: serverName,
		meta:       meta,
		clientFor:  clientFor,
		sink:       sink,
	}
}

// newLiveMCPTool pins a live client: metadata is projected from the client's
// discovered tools and every execution runs on that client.
func newLiveMCPTool(serverName, toolName string, client *Client, sink AttachmentSink) *mcpTool {
	return newMCPTool(serverName, toolMetaOf(client, toolName), func(context.Context) (*Client, error) {
		return client, nil
	}, sink)
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

	if t.serverName != tool.PlaywrightServerName {
		return t.meta.Schema
	}

	var schema map[string]any
	if err := json.Unmarshal(t.meta.Schema, &schema); err != nil || schema == nil {
		schema = map[string]any{"type": "object"}
	}

	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		properties = make(map[string]any)
	}

	properties[tool.BrowserTaskState] = map[string]any{
		"type":        "string",
		"description": "In up to five sentences, say whether the previous action succeeded, failed, or is uncertain based on the current frame; preserve task facts from this frame (names, prices, URLs, counts, exclusions); and name the next immediate goal. Do not describe screen coordinates or layout.",
	}
	schema["properties"] = properties
	required, _ := schema["required"].([]any)
	schema["required"] = append(required, tool.BrowserTaskState)

	encoded, err := json.Marshal(schema)
	if err != nil {
		return t.meta.Schema
	}

	return encoded
}

func (t *mcpTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var args map[string]any
	if err := json.Unmarshal(params, &args); err != nil {
		if t.serverName == tool.PlaywrightServerName {
			return browserTaskStateError(), nil
		}

		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if t.serverName == tool.PlaywrightServerName {
		if !tool.ValidBrowserTaskState(args[tool.BrowserTaskState]) {
			return browserTaskStateError(), nil
		}

		delete(args, tool.BrowserTaskState)
	}

	client, err := t.clientFor(ctx)
	if err != nil {
		return nil, err
	}

	response, err := client.CallTool(ctx, t.meta.Name, args)
	if err != nil {
		return nil, err
	}

	output := append([]string(nil), response.Text...)
	images := make([]llmwire.ImageRef, 0)

	for _, part := range response.Binary {
		if part.DecodeError != nil {
			output = append(output, fmt.Sprintf("%s (%s): invalid base64 data", part.Kind, part.MIME))
			continue
		}

		if t.sink == nil {
			output = append(
				output,
				fmt.Sprintf("%s (%s, %d bytes): attachment storage unavailable", part.Kind, part.MIME, len(part.Data)),
			)

			continue
		}

		attachment, storeErr := t.sink.Store(part)
		if storeErr != nil {
			output = append(
				output,
				fmt.Sprintf(
					"%s (%s, %d bytes): attachment storage unavailable: %v",
					part.Kind,
					part.MIME,
					len(part.Data),
					storeErr,
				),
			)

			continue
		}

		output = append(output, attachment.Note)
		if attachment.Image != nil {
			images = append(images, *attachment.Image)
		}
	}

	return &tool.Result{
		Title:   fmt.Sprintf("MCP: %s/%s", t.serverName, t.meta.Name),
		Output:  strings.Join(output, "\n"),
		Images:  images,
		IsError: response.IsError,
		// MCP is an operator-configured remote/process boundary; its output is
		// never trusted as instructions.
		Untrusted: true,
		Metadata: map[string]any{
			"server": t.serverName,
			"tool":   t.meta.Name,
		},
	}, nil
}

func browserTaskStateError() *tool.Result {
	return &tool.Result{
		IsError: true,
		Output:  "Every playwright tool call requires a non-blank string task_state recording the previous action's outcome, task facts from the current frame, and the next goal.",
	}
}
