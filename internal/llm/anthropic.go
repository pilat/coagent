package llm

import (
	"context"
	"encoding/base64"
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
	client          anthropic.Client
	model           string
	apiKey          string
	usage           *Usage
	reasoningLevel  ReasoningLevel
	maxTokens       int
	contextWindow   int
	pricing         *config.ModelPricing  // catalog-resolved; nil bills the call at zero
	reasoning       *config.ReasoningSpec // catalog-resolved reasoning capability
	inputModalities []string              // catalog-resolved; nil/absent "image" means no pixels are ever sent
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
		client:          client,
		model:           params.Model.ID,
		apiKey:          params.APIKey,
		usage:           &Usage{},
		maxTokens:       params.Model.MaxTokens,
		contextWindow:   params.Model.ContextWindow,
		pricing:         params.Model.Pricing,
		reasoning:       params.Model.Reasoning,
		inputModalities: params.Model.InputModalities,
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

	var lastAssistant *llmwire.Message

	// Consuming the slice from the front keeps every branch advancing
	// unconditionally; a cursor that can strand would loop forever.
	for len(messages) > 0 {
		msg := messages[0]

		switch msg.Role {
		case roleUser:
			if blocks := c.userMessageBlocks(msg); len(blocks) > 0 {
				result = append(result, anthropic.NewUserMessage(blocks...))
			}

			messages = messages[1:]
		case roleAssistant:
			if blocks := c.buildAssistantBlocks(msg); len(blocks) > 0 {
				result = append(result, anthropic.NewAssistantMessage(blocks...))
			}

			lastAssistant = &messages[0]
			messages = messages[1:]
		case roleTool:
			// The provider protocol delivers every tool result of one turn as
			// user content: the whole contiguous run merges into ONE message.
			n := toolRunLength(messages)

			result = append(result, anthropic.NewUserMessage(c.toolRunBlocks(messages[:n], lastAssistant)...))
			messages = messages[n:]
		default:
			// llmwire also carries roleSystem; the Anthropic path folds the
			// system prompt into the request separately.
			messages = messages[1:]
		}
	}

	return result
}

// toolRunLength measures the contiguous tool-result run at the front of the
// slice; the caller only dispatches roleTool messages here, so it is never 0.
func toolRunLength(messages []llmwire.Message) int {
	n := 1
	for n < len(messages) && messages[n].Role == roleTool {
		n++
	}

	return n
}

// toolRunBlocks renders one tool-result run. Blocks follow the assistant call
// list order; results with unknown call ids trail in their arrival order.
func (c *anthropicClient) toolRunBlocks(
	run []llmwire.Message,
	assistant *llmwire.Message,
) []anthropic.ContentBlockParamUnion {
	if assistant != nil && len(assistant.ToolCalls) > 0 {
		rank := make(map[string]int, len(assistant.ToolCalls))
		for idx, tc := range assistant.ToolCalls {
			rank[tc.ID] = idx
		}

		slices.SortStableFunc(run, func(a, b llmwire.Message) int {
			ra, oka := rank[a.ToolCallID]
			rb, okb := rank[b.ToolCallID]

			switch {
			case oka && okb:
				return ra - rb
			case oka:
				return -1
			case okb:
				return 1
			default:
				return 0
			}
		})
	}

	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(run))

	for _, msg := range run {
		blocks = append(blocks, c.toolResultBlock(msg))
	}

	return blocks
}

// userMessageBlocks renders a user message as text (if any) followed by its
// image slots in slice order.
func (c *anthropicClient) userMessageBlocks(msg llmwire.Message) []anthropic.ContentBlockParamUnion {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(msg.Images)+1)

	if msg.Content != "" {
		blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
	}

	for _, ref := range msg.Images {
		blocks = append(blocks, c.imageSlot(ref))
	}

	return blocks
}

// imageSlot resolves one image slot: a base64 block when the gate allows and the
// file materializes, else an inline text placeholder that keeps slot order (D3).
func (c *anthropicClient) imageSlot(ref llmwire.ImageRef) anthropic.ContentBlockParamUnion {
	log := logger.Named("llm.client")

	data, reason := resolveImage(c.inputModalities, ref, log)
	if data == nil {
		return anthropic.NewTextBlock(llmwire.ImagePlaceholder(reason))
	}

	return c.base64ImageBlock(ref.Mime, data)
}

func (c *anthropicClient) base64ImageBlock(mime string, data []byte) anthropic.ContentBlockParamUnion {
	return anthropic.NewImageBlockBase64(string(anthropicImageMediaType(mime)), base64.StdEncoding.EncodeToString(data))
}

// anthropicImageMediaType maps canonical wire MIME strings onto the SDK's media
// type vocabulary; only the four canonical values reach here unfiltered.
func anthropicImageMediaType(mime string) anthropic.Base64ImageSourceMediaType {
	switch mime {
	case llmwire.MimeImageJpeg:
		return anthropic.Base64ImageSourceMediaTypeImageJPEG
	case llmwire.MimeImageGif:
		return anthropic.Base64ImageSourceMediaTypeImageGIF
	case llmwire.MimeImageWebp:
		return anthropic.Base64ImageSourceMediaTypeImageWebP
	default:
		return anthropic.Base64ImageSourceMediaTypeImagePNG
	}
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

// buildToolResultMessage renders a lone tool result as its own user message.
func (c *anthropicClient) buildToolResultMessage(msg llmwire.Message) anthropic.MessageParam {
	return anthropic.NewUserMessage(c.toolResultBlock(msg))
}

// toolResultBlock renders one tool_result; the durable error bit maps onto the
// provider's is_error flag.
func (c *anthropicClient) toolResultBlock(msg llmwire.Message) anthropic.ContentBlockParamUnion {
	if len(msg.Images) == 0 {
		return anthropic.NewToolResultBlock(msg.ToolCallID, toolResultPayloadJSON(msg.Content), msg.ToolError)
	}

	// Image-bearing results nest sibling blocks inside the tool_result's content
	// array (the documented computer-use shape) so setMessageCacheControl keeps
	// stamping the outer block — a sibling outside would move the breakpoint.
	content := []anthropic.ToolResultBlockParamContentUnion{
		{OfText: &anthropic.TextBlockParam{Text: toolResultPayloadJSON(msg.Content)}},
	}

	log := logger.Named("llm.client")

	for _, ref := range msg.Images {
		data, reason := resolveImage(c.inputModalities, ref, log)
		if data != nil {
			content = append(content, anthropic.ToolResultBlockParamContentUnion{
				OfImage: &anthropic.ImageBlockParam{
					Source: anthropic.ImageBlockParamSourceUnion{
						OfBase64: &anthropic.Base64ImageSourceParam{
							Data:      base64.StdEncoding.EncodeToString(data),
							MediaType: anthropicImageMediaType(ref.Mime),
						},
					},
				},
			})
		} else {
			content = append(content, anthropic.ToolResultBlockParamContentUnion{
				OfText: &anthropic.TextBlockParam{Text: llmwire.ImagePlaceholder(reason)},
			})
		}
	}

	return anthropic.ContentBlockParamUnion{
		OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: msg.ToolCallID,
			IsError:   anthropic.Bool(msg.ToolError),
			Content:   content,
		},
	}
}

// toolResultPayloadJSON preserves the historical wire shape: text that parses as
// a JSON object is sent verbatim, anything else is wrapped as {"output": ...}.
func toolResultPayloadJSON(content string) string {
	var resultMap map[string]any
	if err := json.Unmarshal([]byte(content), &resultMap); err != nil {
		resultMap = map[string]any{"output": content}
	}

	resultJSON, err := json.Marshal(resultMap)
	if err != nil {
		resultJSON = []byte(`{"output": "marshal error"}`)
	}

	return string(resultJSON)
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

// mapAnthropicFinish translates the Anthropic stop_reason onto the portable
// llmwire outcome; pause_turn, refusal and anything new are unknown, never stop.
func mapAnthropicFinish(reason anthropic.StopReason) string {
	switch reason {
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
		return llmwire.FinishStop
	case anthropic.StopReasonMaxTokens:
		return llmwire.FinishLength
	case anthropic.StopReasonToolUse:
		return llmwire.FinishToolCalls
	case anthropic.StopReasonPauseTurn, anthropic.StopReasonRefusal,
		anthropic.StopReasonModelContextWindowExceeded:
		return llmwire.FinishUnknown
	default:
		return llmwire.FinishUnknown
	}
}

func (c *anthropicClient) parseResponse(message *anthropic.Message) (*llmwire.Response, error) {
	resp := &llmwire.Response{
		FinishType: llmwire.FinishStop,
	}

	if message == nil {
		return resp, nil
	}

	resp.FinishType = mapAnthropicFinish(message.StopReason)

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
			resp.FinishType = llmwire.FinishToolCalls
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
