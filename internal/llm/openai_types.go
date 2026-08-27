package llm

import (
	"encoding/json"
	"net/http"
	"strings"

	"golang.org/x/oauth2"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

const (
	roleUser            = "user"
	roleAssistant       = "assistant"
	roleTool            = "tool"
	roleSystem          = "system"
	finishTypeToolCalls = "tool_calls"

	msgKeyRole      = "role"
	msgKeyContent   = "content"
	msgKeyType      = "type"
	oaiTypeFunction = "function"
	oaiTypeText     = "text"
)

// oaiRequest represents the request body for OpenAI-compatible chat APIs.
type oaiRequest struct {
	Model              string           `json:"model"`
	Messages           []map[string]any `json:"messages"`
	Tools              []oaiToolDef     `json:"tools,omitempty"`
	ToolChoice         string           `json:"tool_choice,omitempty"`
	Stream             bool             `json:"stream,omitempty"`
	Temperature        float32          `json:"temperature,omitempty"`
	MaxTokens          int              `json:"max_tokens,omitempty"`
	ChatTemplateKwargs map[string]any   `json:"chat_template_kwargs,omitempty"` // DeepSeek thinking
	Thinking           map[string]any   `json:"thinking,omitempty"`             // OpenAI-style (GLM-5, etc.)
	Reasoning          map[string]any   `json:"reasoning,omitempty"`            // OpenAI o1-style reasoning
	Provider           *oaiProvider     `json:"provider,omitempty"`             // OpenRouter-specific provider config
	SessionID          string           `json:"session_id,omitempty"`           // OpenRouter UI session grouping
}

// oaiProvider holds OpenRouter provider configuration.
type oaiProvider struct {
	Only  []string `json:"only,omitempty"`
	Order []string `json:"order,omitempty"`
}

// oaiResponse represents a non-streaming response.
type oaiResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   oaiUsage    `json:"usage"`
}

// oaiChoice represents a completion choice.
type oaiChoice struct {
	Index        int        `json:"index"`
	Message      oaiMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

// oaiUsage represents token usage.
type oaiUsage struct {
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	Cost             *float64 `json:"cost,omitempty"` // OpenRouter returns cost in usage; nil = not returned, 0 = free model
	// Cache tokens from prompt_tokens_details (OpenAI-style)
	PromptTokensDetails *oaiPromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// oaiPromptTokensDetails holds detailed prompt token info.
type oaiPromptTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// oaiMessage represents a message with reasoning_content support.
// Content is json.RawMessage to handle both string and array formats
// (OpenAI newer models may return content as array of content blocks).
type oaiMessage struct {
	Role             string          `json:"role"`
	RawContent       json.RawMessage `json:"content"`
	ToolCalls        []oaiToolCall   `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	ReasoningContent *string         `json:"reasoning_content"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
}

// oaiToolCall represents a tool call.
type oaiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function oaiFunctionCall `json:"function"`
}

// oaiFunctionCall represents function call details.
type oaiFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// oaiToolDef represents a tool definition.
type oaiToolDef struct {
	Type     string         `json:"type"`
	Function oaiFunctionDef `json:"function"`
}

// oaiFunctionDef represents function definition.
type oaiFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// openaiClient provides shared functionality for OpenAI-compatible APIs.
type openaiClient struct {
	baseURL          string
	apiKey           string
	model            string
	httpClient       *http.Client
	provider         string // for log messages and error prefixes
	tokenSource      oauth2.TokenSource
	reasoningLevel   ReasoningLevel
	maxTokens        int
	contextWindow    int
	inputModalities  []string                 // catalog-resolved; nil/absent "image" means no pixels are ever sent
	replayReasoning  bool                     // echo reasoning_details back (OpenRouter's tool-calling contract)
	pricing          *config.ModelPricing     // catalog-resolved; nil bills the call at zero
	reasoning        *config.ReasoningSpec    // catalog-resolved reasoning capability
	openrouterConfig *config.OpenRouterConfig // OpenRouter-specific provider config
}

// content extracts text from RawContent, handling both string and array formats.
// Array format: [{"type":"text","text":"..."},{"type":"text","text":"..."}]
func (m *oaiMessage) content() string {
	if len(m.RawContent) == 0 {
		return ""
	}

	// Try string first (most common)
	var s string
	if err := json.Unmarshal(m.RawContent, &s); err == nil {
		return s
	}

	// Try array of content blocks (OpenAI structured output format)
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}

	if err := json.Unmarshal(m.RawContent, &blocks); err == nil {
		var parts []string

		for _, b := range blocks {
			if b.Type == oaiTypeText && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}

		return strings.Join(parts, "\n")
	}

	// Fallback: return raw as string
	return string(m.RawContent)
}

func newOpenAIClient(
	baseURL, apiKey, provider string,
	tokenSource oauth2.TokenSource,
	entry config.ModelEntry,
) openaiClient {
	// A catalog that carries no output limit leaves max_tokens at 0; the field is
	// omitempty, so the provider picks its own default.
	maxTokens := entry.MaxTokens

	// Providers enforce input + max_tokens <= window, so output gets the complement
	// of the compaction threshold; some catalogs report the whole window as output.
	if reserve := int((1 - llmwire.ContextInputFraction) * float64(entry.ContextWindow)); reserve > 0 {
		maxTokens = min(maxTokens, reserve)
	}

	return openaiClient{
		baseURL:          baseURL,
		apiKey:           apiKey,
		model:            entry.ID,
		httpClient:       &http.Client{}, // per-request deadline comes from the caller's context (retry layer)
		provider:         provider,
		tokenSource:      tokenSource,
		maxTokens:        maxTokens,
		contextWindow:    entry.ContextWindow,
		inputModalities:  entry.InputModalities,
		pricing:          entry.Pricing,
		reasoning:        entry.Reasoning,
		openrouterConfig: entry.OpenRouterConfig,
	}
}
