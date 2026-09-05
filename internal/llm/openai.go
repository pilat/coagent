package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/oauth2"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

// openAISuffix is appended to system prompts for OpenAI-compatible providers
// to enforce native function calling instead of XML tool call output.
const openAISuffix = "IMPORTANT: You MUST use the native function calling API to invoke tools. " +
	"NEVER output tool calls as XML, HTML, or any other markup in your text response. " +
	"If you want to call a tool, use the tool_calls mechanism provided by the API.\n\n" +
	"EXAMPLE - Correct way to call a tool:\n" +
	"When you need to call a tool, the API will handle it. You should NOT output XML like <read> or <write>. " +
	"Instead, the system will provide tool definitions and you respond with tool_calls in the proper JSON format. " +
	"For example, to read a file, you would use tool_calls with: {\"name\": \"read\", \"arguments\": {\"file_path\": \"/path/to/file\"}}\n\n" +
	"REMEMBER: Never use XML tags like <function_calls>, <read>, <write>, etc. These will NOT work. " +
	"Always use the native tool_calls API."

var _ Client = (*openAICompatibleClient)(nil)

// openAICompatibleClient wraps openaiClient for any OpenAI-compatible API.
// Works with DeepSeek, GLM, OpenRouter, and other OpenAI-compatible providers.
type openAICompatibleClient struct {
	openaiClient
	// isDeepSeek indicates DeepSeek models that need specific workarounds
	// (system prompt as user message, chat_template_kwargs for thinking)
	isDeepSeek bool
	// isAnthropic indicates Anthropic models via OpenRouter that support
	// cache_control markers for prompt caching
	isAnthropic bool
	// isOpenRouter enables OpenRouter-specific features (session_id for UI grouping)
	isOpenRouter bool
	// sessionID for OpenRouter UI session grouping
	sessionID string
	// nativeSearch enables the OpenRouter server-tool web-search passthrough.
	// Injection happens per request and only alongside client-side tools.
	nativeSearch bool
}

type openAICompatibleParams struct {
	BaseURL      string
	APIKey       string
	Model        config.ModelEntry  // catalog-enriched: limits, pricing, reasoning capability
	TokenSource  oauth2.TokenSource // optional: for Google SA auto-refresh (overrides APIKey)
	IsOpenRouter bool               // enables OpenRouter-specific features (session_id, reasoning)
	NativeSearch bool               // OpenRouter server-tool web-search passthrough
}

// newOpenAICompatibleClient creates a new OpenAI-compatible client.
// Works with DeepSeek, OpenRouter, and any other OpenAI-compatible provider.
func newOpenAICompatibleClient(params openAICompatibleParams) (Client, error) {
	if params.APIKey == "" && params.TokenSource == nil {
		return nil, errors.New("API key or token source is required")
	}

	if params.BaseURL == "" {
		return nil, errors.New("base URL is required for OpenAI-compatible provider")
	}

	model := params.Model.ID
	if model == "" {
		return nil, errors.New("model is required")
	}

	// Detect DeepSeek models (need specific workarounds regardless of endpoint)
	isDeepSeek := strings.HasPrefix(model, "deepseek-ai/") ||
		strings.HasPrefix(strings.ToLower(model), "deepseek")

	// Detect Anthropic models via OpenRouter (support cache_control markers)
	isAnthropic := strings.HasPrefix(model, "anthropic/")

	provider := "openai-compatible"
	if params.IsOpenRouter {
		provider = "openrouter"
	} else if isDeepSeek {
		provider = "deepseek"
	} else if strings.HasPrefix(model, "zai-org/") {
		provider = "glm"
	}

	if params.Model.MaxTokens == 0 && isAnthropic {
		return nil, fmt.Errorf(
			"model %q: the Anthropic backend mandates max_tokens, which its catalog does not carry",
			model,
		)
	}

	client := newOpenAIClient(params.BaseURL, params.APIKey, provider, params.TokenSource, params.Model)
	client.replayReasoning = params.IsOpenRouter

	return &openAICompatibleClient{
		openaiClient: client,
		isDeepSeek:   isDeepSeek,
		isAnthropic:  isAnthropic,
		isOpenRouter: params.IsOpenRouter,
		nativeSearch: params.IsOpenRouter && params.NativeSearch,
	}, nil
}

func (c *openAICompatibleClient) Provider() string {
	return c.provider
}

func (c *openAICompatibleClient) SetSessionID(id string) {
	if c.isOpenRouter {
		c.sessionID = id
	}
}

func (c *openAICompatibleClient) Chat(
	ctx context.Context,
	systemPrompt string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	reqBody := oaiRequest{
		Model:    c.model,
		Messages: c.convertMessages(messages),
		Stream:   c.isOpenRouter,
	}

	if c.isOpenRouter && c.sessionID != "" {
		reqBody.SessionID = c.sessionID
	}

	if c.openrouterConfig != nil && (len(c.openrouterConfig.Only) > 0 || len(c.openrouterConfig.Order) > 0) {
		reqBody.Provider = &oaiProvider{
			Only:  c.openrouterConfig.Only,
			Order: c.openrouterConfig.Order,
		}
	}

	// OpenRouter normalizes max_tokens for all providers (including OpenAI).
	// Do NOT send max_completion_tokens — it's not in OpenRouter's supported_parameters.
	reqBody.MaxTokens = llmwire.ApplyChatOptions(opts).EffectiveMaxTokens(c.maxTokens)

	// DeepSeek breaks function calling when a system prompt is used.
	// Workaround: inject system prompt as a user message prefix.
	// Other providers (GLM, OpenRouter, etc.) use standard system message.
	if systemPrompt != "" {
		fullPrompt := systemPrompt + "\n\n" + openAISuffix
		if c.isDeepSeek {
			reqBody.Messages = append([]map[string]any{
				{msgKeyRole: roleUser, msgKeyContent: fullPrompt},
				{msgKeyRole: roleAssistant, msgKeyContent: "Understood. I will follow these instructions."},
			}, reqBody.Messages...)
		} else if c.isAnthropic {
			// Anthropic via OpenRouter: use content blocks with cache_control
			// for explicit per-block caching (works across all providers
			// including Bedrock and Vertex, unlike top-level cache_control).
			reqBody.Messages = append([]map[string]any{
				{msgKeyRole: roleSystem, msgKeyContent: []map[string]any{
					{
						msgKeyType:      oaiTypeText,
						oaiTypeText:     fullPrompt,
						"cache_control": map[string]any{msgKeyType: "ephemeral"},
					},
				}},
			}, reqBody.Messages...)
		} else {
			// OpenRouter translates "system" to "developer" automatically for
			// OpenAI post-o-series models. No need to handle it ourselves.
			reqBody.Messages = append([]map[string]any{
				{msgKeyRole: roleSystem, msgKeyContent: fullPrompt},
			}, reqBody.Messages...)
		}
	}

	// Anthropic via OpenRouter: add cache_control breakpoints to user messages
	// for cross-turn caching (system prompt + conversation prefix stay cached).
	if c.isAnthropic {
		addAnthropicCacheMarkers(reqBody.Messages)
	}

	oaiTools := c.convertTools(tools)
	if len(oaiTools) > 0 {
		reqBody.Tools = oaiTools
		reqBody.ToolChoice = "auto"

		// OpenRouter executes the server-side search inside the same request
		// (its interception loop, capped by max_tool_calls). Tool-less requests
		// — e.g. the compaction summarizer — carry no injection: they must
		// neither pay for nor fail on server-side searches. The injected entry
		// has no parameters in v1 (defaults; engine auto).
		if c.nativeSearch {
			reqBody.Tools = append(reqBody.Tools, oaiToolDef{Type: openRouterServerSearch})
			reqBody.MaxToolCalls = maxNativeSearchToolCalls
		}
	}

	// Arbitrary OpenAI-compatible endpoints can 400 on an unknown field, so only
	// OpenRouter — which translates the param per provider — ever sees it.
	if c.isDeepSeek {
		reqBody.ChatTemplateKwargs = map[string]any{"thinking": true}
	} else if effort, ok := c.effortParam(); ok {
		reqBody.Reasoning = map[string]any{"effort": effort}
	}

	return c.makeRequest(ctx, reqBody)
}

// effortParam reports the level to put on the wire, clamped to what the model
// accepts. A model exposing no effort selector gets nothing: the gateway would
// otherwise map our level onto one we never chose.
func (c *openAICompatibleClient) effortParam() (string, bool) {
	if !c.isOpenRouter || c.reasoning == nil || !c.reasoning.Supported {
		return "", false
	}

	if len(c.reasoning.Efforts) == 0 && !c.reasoning.AnyEffort {
		return "", false
	}

	return catalog.ClampEffort(c.GetReasoningLevel(), c.reasoning.Efforts), true
}

// addAnthropicCacheMarkers adds cache_control breakpoints to messages for
// Anthropic prompt caching via OpenRouter. Uses up to 3 breakpoints (of 4 allowed):
//
//  1. System prompt — large, static across entire session (set in Chat)
//  2. First user message — CLAUDE.md preferences, static across turns
//  3. Sliding window — second-to-last user message, so the conversation
//     prefix stays cached and only the latest turn is re-processed
func addAnthropicCacheMarkers(messages []map[string]any) {
	cacheControl := map[string]any{msgKeyType: "ephemeral"}

	// Breakpoint 2: first user message (CLAUDE.md preferences)
	firstUserIdx := -1

	for i, msg := range messages {
		role, _ := msg[msgKeyRole].(string)
		if role == roleUser {
			firstUserIdx = i
			break
		}
	}

	if firstUserIdx >= 0 {
		applyCacheControl(messages, firstUserIdx, cacheControl)
	}

	// Breakpoint 3: sliding window — find the second-to-last user message.
	// This creates a cache boundary so that only the latest turn is re-processed.
	lastUserIdx := -1
	secondLastUserIdx := -1

	for i, msg := range slices.Backward(messages) {
		role, _ := msg[msgKeyRole].(string)
		if role != roleUser {
			continue
		}

		if lastUserIdx == -1 {
			lastUserIdx = i
		} else {
			secondLastUserIdx = i
			break
		}
	}

	// Only add sliding breakpoint if it's a different message from the first user
	if secondLastUserIdx > firstUserIdx {
		applyCacheControl(messages, secondLastUserIdx, cacheControl)
	}
}

// applyCacheControl converts a message's content to content blocks with cache_control.
// If content is already a content block array, adds cache_control to the last block.
func applyCacheControl(messages []map[string]any, idx int, cacheControl map[string]any) {
	msg := messages[idx]

	switch content := msg[msgKeyContent].(type) {
	case string:
		messages[idx][msgKeyContent] = []map[string]any{
			{msgKeyType: oaiTypeText, oaiTypeText: content, "cache_control": cacheControl},
		}
	case []map[string]any:
		if len(content) > 0 {
			content[len(content)-1]["cache_control"] = cacheControl
		}
	}
}
