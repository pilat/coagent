package llm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

// Effort vocabulary. Which of these a given model actually accepts is a catalog
// fact, not a constant — see config.ReasoningSpec.Efforts.
const (
	ReasoningNone    ReasoningLevel = "none"
	ReasoningMinimal ReasoningLevel = "minimal"
	ReasoningLow     ReasoningLevel = "low"
	ReasoningMedium  ReasoningLevel = "medium"
	ReasoningHigh    ReasoningLevel = "high"
	ReasoningXHigh   ReasoningLevel = "xhigh"
	ReasoningMax     ReasoningLevel = "max"
)

type ReasoningLevel string

type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CacheTokens      int // Cached prompt tokens (cache read hits)
	CacheWriteTokens int // Tokens written to cache (first request establishing cache)
	CostUSD          float64
}

// Client is the interface for LLM clients.
//
// Chat populates llmwire.Response.CostUSD and llmwire.Response.Usage with token
// accounting for the call. Callers that need usage info read it from the
// response — there is no separate usage return.
//
// Usage contract (uniform across providers): PromptTokens is the total input the
// model processed, cache included. CacheTokens (read) and CacheWriteTokens are
// subsets of it, so PromptTokens >= CacheTokens + CacheWriteTokens always holds.
// Anthropic's native input_tokens excludes cache, so its extractor sums
// input + cache_read + cache_creation to satisfy this.
type Client interface {
	// Chat sends one request. Options narrow that request only — a client's own
	// configuration is the ceiling, never raised by an option.
	Chat(
		ctx context.Context,
		systemPrompt string,
		messages []llmwire.Message,
		tools []llmwire.ToolSchema,
		opts ...llmwire.ChatOption,
	) (*llmwire.Response, error)
	Model() string
	APIKey() string
	Close() error
	// Provider returns the provider name (used for logging and pricing).
	Provider() string
	// ContextWindow returns the model's catalog-resolved context window in tokens.
	// Startup validation guarantees it is set for every configured model.
	ContextWindow() int
	// SetReasoningLevel sets the reasoning effort level (low, medium, high).
	SetReasoningLevel(level string)
	// GetReasoningLevel returns the current reasoning effort level.
	GetReasoningLevel() string
	// SetSessionID sets the session ID for OpenRouter UI grouping.
	SetSessionID(id string)
}

// NewClient creates a new LLM client based on config.
// Looks up the model's provider and uses the provider's driver to instantiate the correct client.
func NewClient(cfg *config.Config) (Client, error) {
	model := cfg.Model
	if model == "" {
		model = cfg.DefaultModel()
	}

	if model == "" {
		return nil, errors.New("no model configured")
	}

	return newClientForModel(cfg, model)
}

// NewClientWithModel creates a new LLM client with an explicit model override.
// If model is empty, uses the main model.
func NewClientWithModel(cfg *config.Config, model string) (Client, error) {
	if model == "" {
		return NewClient(cfg)
	}

	return newClientForModel(cfg, model)
}

// newClientForModel creates a client for the given model using provider/driver lookup.
func newClientForModel(cfg *config.Config, model string) (Client, error) {
	if cfg.UnifiedConfig == nil {
		return nil, errors.New("no unified config loaded")
	}

	modelEntry, ok := findModel(cfg.UnifiedConfig, model)
	if !ok {
		return nil, fmt.Errorf("model %q not found in config", model)
	}

	provider, ok := cfg.UnifiedConfig.Providers[modelEntry.Provider]
	if !ok {
		return nil, fmt.Errorf("model %q references unknown provider %q", model, modelEntry.Provider)
	}

	driver, ok := defaultDrivers[provider.Driver]
	if !ok {
		return nil, fmt.Errorf(
			"model %q: provider %q has unsupported driver %q",
			model,
			modelEntry.Provider,
			provider.Driver,
		)
	}

	// The native-search decision rides the client: explicit REST config or an
	// explicit disable suppresses it, an unconfigured section falls back to
	// native for OR-driver models (config.SearchNativeActive).
	opts := DriverClientOpts{NativeSearch: cfg.UnifiedConfig.SearchNativeActive(model)}

	inner, err := driver.NewClient(provider, modelEntry, opts)
	if err != nil {
		return nil, fmt.Errorf("model %q: %w", model, err)
	}

	return newRetryableClient(inner, time.Duration(modelEntry.TimeoutSec)*time.Second), nil
}

func findModel(uc *config.UnifiedConfig, model string) (config.ModelEntry, bool) {
	for _, m := range uc.Models {
		if m.ID == model {
			return m, true
		}
	}

	return config.ModelEntry{}, false
}
