package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
)

var _ Client = (*anthropicClient)(nil)

type anthropicClient struct {
	client         anthropic.Client
	model          string
	apiKey         string
	usage          *Usage
	reasoningLevel ReasoningLevel
	maxTokens      int
	contextWindow  int
	pricing        *config.ModelPricing  // catalog-resolved; nil bills the call at zero
	reasoning      *config.ReasoningSpec // catalog-resolved reasoning capability
}

// anthropicParams holds parameters for creating an Anthropic client.
type anthropicParams struct {
	APIKey string
	Model  config.ModelEntry // catalog-enriched: limits, pricing, reasoning capability
}

// newAnthropicClient creates a new Anthropic client.
func newAnthropicClient(params anthropicParams) (Client, error) {
	client := anthropic.NewClient(
		option.WithAPIKey(params.APIKey),
		option.WithHeader("User-Agent", "pilat/coagent"),
	)

	if params.Model.MaxTokens == 0 {
		return nil, fmt.Errorf(
			"model %q: the Anthropic API mandates max_tokens, which its catalog does not carry",
			params.Model.ID,
		)
	}

	return &anthropicClient{
		client:        client,
		model:         params.Model.ID,
		apiKey:        params.APIKey,
		usage:         &Usage{},
		maxTokens:     params.Model.MaxTokens,
		contextWindow: params.Model.ContextWindow,
		pricing:       params.Model.Pricing,
		reasoning:     params.Model.Reasoning,
	}, nil
}

func (c *anthropicClient) Provider() string {
	return "anthropic"
}

func (c *anthropicClient) Chat(
	ctx context.Context,
	systemPrompt string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	log := logger.Ctx(ctx).Named("llm.client")
	log.Debug("anthropic_chat", zap.Int("messages", len(messages)), zap.Int("tools", len(tools)))

	maxTokens := llmwire.ApplyChatOptions(opts).EffectiveMaxTokens(c.maxTokens)
	params := c.buildMessageParams(systemPrompt, messages, tools, maxTokens)

	log.Debug("calling_api", zap.String("provider", "anthropic"), zap.String("model", c.model))

	message, err := c.streamMessage(ctx, params)
	if err != nil {
		return nil, err
	}

	usage := c.extractAnthropicUsage(message)

	log.Debug("response", zap.String("provider", "anthropic"), zap.Int("content_blocks", len(message.Content)),
		zap.Int("usage_input", usage.PromptTokens), zap.Int("usage_output", usage.CompletionTokens),
		zap.Int("cache_read", usage.CacheTokens), zap.Int("cache_write", usage.CacheWriteTokens))

	resp, err := c.parseResponse(message)
	if err != nil {
		return nil, err
	}

	resp.CostUSD = usage.CostUSD
	resp.Usage = &llmwire.MessageUsage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		CacheTokens:      usage.CacheTokens,
		CacheWriteTokens: usage.CacheWriteTokens,
	}

	return resp, nil
}

func (c *anthropicClient) Close() error {
	return nil
}

func (c *anthropicClient) Model() string {
	return c.model
}

func (c *anthropicClient) APIKey() string {
	return c.apiKey
}

func (c *anthropicClient) ContextWindow() int {
	return c.contextWindow
}

func (c *anthropicClient) SetReasoningLevel(level string) {
	c.reasoningLevel = ReasoningLevel(level)
}

func (c *anthropicClient) GetReasoningLevel() string {
	if c.reasoningLevel == "" {
		return string(ReasoningMedium)
	}

	return string(c.reasoningLevel)
}

func (c *anthropicClient) SetSessionID(id string) {
	// No-op for Anthropic; session_id is OpenRouter-specific
}

func (c *anthropicClient) buildMessageParams(
	systemPrompt string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	maxTokens int,
) anthropic.MessageNewParams {
	anthropicMessages := c.convertMessages(messages)
	addAnthropicNativeCacheMarkers(anthropicMessages)

	params := anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: int64(maxTokens),
		Messages:  anthropicMessages,
	}

	if systemPrompt != "" {
		block := anthropic.TextBlockParam{Text: systemPrompt}
		block.CacheControl = anthropic.NewCacheControlEphemeralParam()
		params.System = []anthropic.TextBlockParam{block}
	}

	if len(tools) > 0 {
		if anthropicTools := c.convertTools(tools); len(anthropicTools) > 0 {
			params.Tools = anthropicTools
		}
	}

	// The budget is sized against the cap this request actually carries: the API
	// rejects budget >= max_tokens, so a capped call must shrink its thinking too.
	if thinking := buildThinkingParams(c.reasoning, c.effortLevel(), maxTokens); thinking.Enabled {
		params.Thinking = thinking.Thinking
		params.OutputConfig.Effort = thinking.Effort
	}

	return params
}

func (c *anthropicClient) effortLevel() ReasoningLevel {
	if c.reasoningLevel == "" {
		return ReasoningMedium
	}

	return c.reasoningLevel
}

func (c *anthropicClient) convertMessages(messages []llmwire.Message) []anthropic.MessageParam {
	result := make([]anthropic.MessageParam, 0, len(messages))

	for _, msg := range messages {
		switch msg.Role {
		case roleUser:
			if msg.Content != "" {
				result = append(result, anthropic.NewUserMessage(
					anthropic.NewTextBlock(msg.Content),
				))
			}
		case roleAssistant:
			if blocks := c.buildAssistantBlocks(msg); len(blocks) > 0 {
				result = append(result, anthropic.NewAssistantMessage(blocks...))
			}
		case roleTool:
			result = append(result, c.buildToolResultMessage(msg))
		}
	}

	return result
}

func (c *anthropicClient) buildAssistantBlocks(msg llmwire.Message) []anthropic.ContentBlockParamUnion {
	thinking := c.replayThinkingBlocks(msg)

	capacity := len(thinking) + len(msg.ToolCalls)
	if msg.Content != "" {
		capacity++
	}

	// Thinking blocks must lead the message, exactly where the model emitted them.
	blocks := make([]anthropic.ContentBlockParamUnion, 0, capacity)
	blocks = append(blocks, thinking...)

	if msg.Content != "" {
		blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
	}

	for _, tc := range msg.ToolCalls {
		var input map[string]any
		if err := json.Unmarshal(tc.Arguments, &input); err != nil {
			input = map[string]any{}
		}

		blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Name))
	}

	return blocks
}

func (c *anthropicClient) buildToolResultMessage(msg llmwire.Message) anthropic.MessageParam {
	var resultMap map[string]any
	if err := json.Unmarshal([]byte(msg.Content), &resultMap); err != nil {
		resultMap = map[string]any{"output": msg.Content}
	}

	resultJSON, err := json.Marshal(resultMap)
	if err != nil {
		resultJSON = []byte(`{"output": "marshal error"}`)
	}

	return anthropic.NewUserMessage(
		anthropic.NewToolResultBlock(msg.ToolCallID, string(resultJSON), false),
	)
}

func (c *anthropicClient) streamMessage(
	ctx context.Context,
	params anthropic.MessageNewParams,
) (*anthropic.Message, error) {
	stream := c.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	var message anthropic.Message

	for stream.Next() {
		if err := message.Accumulate(stream.Current()); err != nil {
			return nil, fmt.Errorf("anthropic stream accumulate: %w", err)
		}
	}

	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("anthropic api error: %w", err)
	}

	return &message, nil
}

func (c *anthropicClient) extractAnthropicUsage(message *anthropic.Message) Usage {
	// input_tokens is only the uncached remainder; PromptTokens is cache-inclusive
	// (see llmwire.MessageUsage), so add the two cache breakdowns back in.
	promptTokens := int(message.Usage.InputTokens +
		message.Usage.CacheReadInputTokens +
		message.Usage.CacheCreationInputTokens)

	usage := Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: int(message.Usage.OutputTokens),
		TotalTokens:      promptTokens + int(message.Usage.OutputTokens),
		CacheTokens:      int(message.Usage.CacheReadInputTokens),
		CacheWriteTokens: int(message.Usage.CacheCreationInputTokens),
	}
	usage.CostUSD = estimateCost(usage, c.pricing)

	return usage
}

func (c *anthropicClient) convertTools(tools []llmwire.ToolSchema) []anthropic.ToolUnionParam {
	if len(tools) == 0 {
		return nil
	}

	result := make([]anthropic.ToolUnionParam, 0, len(tools))

	for _, t := range tools {
		schema, ok := parseParamsSchema(t.Name, t.Parameters)
		if !ok {
			continue
		}

		properties, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)

		requiredStrings := make([]string, 0, len(required))

		for _, r := range required {
			if s, ok := r.(string); ok {
				requiredStrings = append(requiredStrings, s)
			}
		}

		inputSchema := anthropic.ToolInputSchemaParam{
			Properties: properties,
		}
		if len(requiredStrings) > 0 {
			inputSchema.Required = requiredStrings
		}

		toolParam := anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: inputSchema,
		}

		result = append(result, anthropic.ToolUnionParam{
			OfTool: &toolParam,
		})
	}

	return result
}

func (c *anthropicClient) parseResponse(message *anthropic.Message) (*llmwire.Response, error) {
	resp := &llmwire.Response{
		FinishType: "stop",
	}

	if message == nil {
		return resp, nil
	}

	var thinking []anthropicThinkingBlock

	for _, block := range message.Content {
		switch block := block.AsAny().(type) {
		case anthropic.TextBlock:
			resp.Text += block.Text
		case anthropic.ThinkingBlock:
			thinking = append(thinking, anthropicThinkingBlock{
				Type:      thinkingBlockType,
				Thinking:  block.Thinking,
				Signature: block.Signature,
			})
		case anthropic.RedactedThinkingBlock:
			thinking = append(thinking, anthropicThinkingBlock{
				Type: redactedThinkingBlockType,
				Data: block.Data,
			})
		case anthropic.ToolUseBlock:
			inputJSON, err := json.Marshal(block.Input)
			if err != nil {
				inputJSON = []byte("{}")
			}

			resp.ToolCalls = append(resp.ToolCalls, llmwire.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: inputJSON,
			})
			resp.FinishType = finishTypeToolCalls
		}
	}

	if payload, err := json.Marshal(thinking); err == nil && len(thinking) > 0 {
		resp.ReasoningRaw = wrapReasoning(c.model, payload)
	}

	return resp, nil
}

// addAnthropicNativeCacheMarkers adds cache_control breakpoints to messages
// for Anthropic prompt caching. Uses up to 2 message breakpoints (of 4 allowed;
// system prompt uses 1):
//  1. First user message — CLAUDE.md preferences, static across turns
//  2. Second-to-last user message — sliding window so conversation prefix stays cached
func addAnthropicNativeCacheMarkers(messages []anthropic.MessageParam) {
	cc := anthropic.NewCacheControlEphemeralParam()

	// Breakpoint 1: first user message
	firstUserIdx := -1

	for i := range messages {
		if messages[i].Role == anthropic.MessageParamRoleUser {
			firstUserIdx = i
			break
		}
	}

	if firstUserIdx >= 0 {
		setMessageCacheControl(&messages[firstUserIdx], cc)
	}

	// Breakpoint 2: sliding window — second-to-last user message
	lastUserIdx := -1
	secondLastUserIdx := -1

	for i, msg := range slices.Backward(messages) {
		if msg.Role != anthropic.MessageParamRoleUser {
			continue
		}

		if lastUserIdx == -1 {
			lastUserIdx = i
		} else {
			secondLastUserIdx = i
			break
		}
	}

	if secondLastUserIdx > firstUserIdx {
		setMessageCacheControl(&messages[secondLastUserIdx], cc)
	}
}

// setMessageCacheControl sets cache_control on the last content block of a message.
func setMessageCacheControl(msg *anthropic.MessageParam, cc anthropic.CacheControlEphemeralParam) {
	if len(msg.Content) == 0 {
		return
	}

	last := &msg.Content[len(msg.Content)-1]
	if last.OfText != nil {
		last.OfText.CacheControl = cc
	} else if last.OfToolResult != nil {
		last.OfToolResult.CacheControl = cc
	}
}
