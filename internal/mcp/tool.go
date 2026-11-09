package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

var (
	_ tool.UntrustedSource = (*mcpTool)(nil)
	_ tool.PromptDeclarer  = (*mcpTool)(nil)
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

// Profile lets the owner of one server's tool surface adapt its tools.
// Gate receives nil args when decodeErr is non-nil; a nil result means forward the call.
type Profile interface {
	Admit(meta ToolMeta) bool
	Schema(schema json.RawMessage) json.RawMessage
	Gate(args map[string]any, decodeErr error) *tool.Result
}

// SearchOrder reports whether the stack's owner treats a tool as a web search
// tool, and its position among them.
type SearchOrder func(toolID string) (int, bool)

// mcpTool keeps the metadata discovered for one stack and calls its live client.
type mcpTool struct {
	serverName  string
	meta        ToolMeta
	clientFor   func(ctx context.Context) (*Client, error)
	sink        AttachmentSink
	profile     Profile
	search      bool
	searchOrder int
}

// newMCPTool builds a direct MCP tool from discovered metadata.
func newMCPTool(
	serverName string,
	meta ToolMeta,
	clientFor func(ctx context.Context) (*Client, error),
	sink AttachmentSink,
	profile Profile,
) *mcpTool {
	return &mcpTool{
		serverName: serverName,
		meta:       meta,
		clientFor:  clientFor,
		sink:       sink,
		profile:    profile,
	}
}

// newLiveMCPTool pins a live client: metadata is projected from the client's
// discovered tools and every execution runs on that client.
func newLiveMCPTool(serverName, toolName string, client *Client, sink AttachmentSink, profile Profile) *mcpTool {
	return newMCPTool(serverName, toolMetaOf(client, toolName), func(context.Context) (*Client, error) {
		return client, nil
	}, sink, profile)
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

func (t *mcpTool) UntrustedSource() {}

func (t *mcpTool) Description() string {
	return t.meta.Description
}

func (t *mcpTool) PromptDeclaration() tool.PromptDeclaration {
	return tool.PromptDeclaration{Search: t.search, SearchOrder: t.searchOrder}
}

func (t *mcpTool) Parameters() json.RawMessage {
	if len(t.meta.Schema) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}

	if t.profile == nil {
		return t.meta.Schema
	}

	return t.profile.Schema(t.meta.Schema)
}

func (t *mcpTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var args map[string]any

	err := json.Unmarshal(params, &args)
	if t.profile != nil {
		if result := t.profile.Gate(args, err); result != nil {
			return result, nil
		}
	} else if err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
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
