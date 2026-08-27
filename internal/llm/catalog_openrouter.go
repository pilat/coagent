package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
)

// perTokenToPerMillion converts OpenRouter's per-single-token string prices to
// the per-1M units ModelPricing uses everywhere else.
const perTokenToPerMillion = 1_000_000

const (
	openRouterReasoningParam = "reasoning"

	// defaultOpenRouterURL serves when the provider entry pins no base_url. The
	// endpoint belongs to this driver, not to the shared transport.
	defaultOpenRouterURL = "https://openrouter.ai/api/v1/models"
)

// openRouterSource derives the models endpoint from the provider's base_url, so a
// self-hosted gateway is read from its own catalog rather than the public one.
func openRouterSource(baseURL string) catalog.Source {
	url := defaultOpenRouterURL
	if baseURL != "" {
		url = strings.TrimRight(baseURL, "/") + "/models"
	}

	return catalog.Source{
		URL:       url,
		CacheName: catalog.CacheName("openrouter", url),
		Validate: func(body []byte) error {
			_, err := parseOpenRouter(body)

			return err
		},
	}
}

type (
	openRouterResponse struct {
		Data []openRouterModel `json:"data"`
	}

	openRouterModel struct {
		ID                  string               `json:"id"`
		Name                string               `json:"name"`
		ContextLength       int                  `json:"context_length"`
		TopProvider         openRouterTop        `json:"top_provider"`
		Pricing             openRouterPricing    `json:"pricing"`
		SupportedParameters []string             `json:"supported_parameters"`
		Reasoning           *openRouterReasoning `json:"reasoning"`
		Architecture        *openRouterArch      `json:"architecture"`
	}

	openRouterArch struct {
		Modality string `json:"modality"` // arrow form, e.g. "text+image->text+image"
	}

	openRouterReasoning struct {
		Mandatory      bool   `json:"mandatory"`
		DefaultEffort  string `json:"default_effort"`
		DefaultEnabled bool   `json:"default_enabled"`
		// Raw, because absent and null mean different things here and both decode
		// to a nil slice: absent = no effort selector, null = every level accepted.
		SupportedEfforts json.RawMessage `json:"supported_efforts"`
	}

	openRouterTop struct {
		ContextLength       int  `json:"context_length"`
		MaxCompletionTokens *int `json:"max_completion_tokens"`
	}

	openRouterPricing struct {
		Prompt          string `json:"prompt"`
		Completion      string `json:"completion"`
		InputCacheRead  string `json:"input_cache_read"`
		InputCacheWrite string `json:"input_cache_write"`
	}
)

// parseOpenRouter converts the /api/v1/models payload into id → spec.
func parseOpenRouter(body []byte) (map[string]catalog.ModelSpec, error) {
	var raw openRouterResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse openrouter catalog: %w", err)
	}

	if len(raw.Data) == 0 {
		return nil, errors.New("parse openrouter catalog: empty model list")
	}

	models := make(map[string]catalog.ModelSpec, len(raw.Data))
	for _, m := range raw.Data {
		models[m.ID] = m.toSpec()
	}

	return models, nil
}

func (m openRouterModel) toSpec() catalog.ModelSpec {
	// top_provider reflects the endpoint that actually serves the request; the
	// model-level length is the optimistic max across providers.
	contextWindow := m.TopProvider.ContextLength
	if contextWindow == 0 {
		contextWindow = m.ContextLength
	}

	maxTokens := 0
	if m.TopProvider.MaxCompletionTokens != nil {
		maxTokens = *m.TopProvider.MaxCompletionTokens
	}

	modality := ""
	if m.Architecture != nil {
		modality = m.Architecture.Modality
	}

	return catalog.ModelSpec{
		Name:            m.Name,
		Source:          "openrouter",
		ContextWindow:   contextWindow,
		MaxTokens:       maxTokens,
		InputModalities: parseOpenRouterModality(modality),
		Pricing: &config.ModelPricing{
			InputPrice:      perMillion(m.Pricing.Prompt),
			OutputPrice:     perMillion(m.Pricing.Completion),
			CacheReadPrice:  perMillion(m.Pricing.InputCacheRead),
			CacheWritePrice: perMillion(m.Pricing.InputCacheWrite),
		},
		Reasoning: m.reasoningSpec(),
	}
}

// reasoningSpec reads the per-model reasoning contract. The dedicated object is
// authoritative; supported_parameters only still answers for dynamic router ids,
// which carry the parameter without an object.
func (m openRouterModel) reasoningSpec() *config.ReasoningSpec {
	if m.Reasoning == nil {
		return &config.ReasoningSpec{Supported: slices.Contains(m.SupportedParameters, openRouterReasoningParam)}
	}

	spec := &config.ReasoningSpec{Supported: true, Default: m.Reasoning.DefaultEffort}

	switch efforts, declared := m.Reasoning.efforts(); {
	case !declared:
		// No selector: the model reasons on its own terms and an effort we send
		// would only be mapped onto something we never chose.
	case efforts == nil:
		spec.AnyEffort = true
		spec.NativeEffort = true
	default:
		spec.Efforts = catalog.SortEfforts(efforts)
		spec.NativeEffort = true
	}

	return spec
}

// efforts reports the allowlist and whether the model declares one at all. A
// declared-but-nil list is the catalog's "any gateway level" signal.
func (r openRouterReasoning) efforts() ([]string, bool) {
	if len(r.SupportedEfforts) == 0 {
		return nil, false
	}

	var levels []string
	if err := json.Unmarshal(r.SupportedEfforts, &levels); err != nil {
		return nil, false
	}

	return levels, true
}

func perMillion(raw string) float64 {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}

	return v * perTokenToPerMillion
}

// parseOpenRouterModality splits the arrow form "text+image->text+image" into
// its input modality list. Absent or malformed input returns nil — the gate
// must fail closed, never invent modalities.
func parseOpenRouterModality(raw string) []string {
	input, _, found := strings.Cut(raw, "->")
	if !found || input == "" {
		return nil
	}

	parts := strings.Split(input, "+")
	modalities := make([]string, 0, len(parts))

	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			modalities = append(modalities, p)
		}
	}

	if len(modalities) == 0 {
		return nil
	}

	return modalities
}
