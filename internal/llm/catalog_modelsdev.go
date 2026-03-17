package llm

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
)

const (
	// modelsDevURL is the community catalog this driver family reads. It lives
	// here, not in the transport, because the source is the driver's business.
	modelsDevURL       = "https://models.dev/api.json"
	modelsDevCacheName = "modelsdev.json"

	reasoningTypeEffort = "effort"
	reasoningTypeBudget = "budget_tokens"

	// defaultBudgetMin is the floor applied when a budget-based model's catalog
	// entry omits its minimum.
	defaultBudgetMin = 1024
)

// modelsDevSource is what the fetcher needs to retrieve and validate this catalog.
var modelsDevSource = catalog.Source{
	URL:       modelsDevURL,
	CacheName: modelsDevCacheName,
	Validate: func(body []byte) error {
		_, err := parseModelsDev(body)

		return err
	},
}

type (
	modelsDevSection struct {
		Models map[string]modelsDevModel `json:"models"`
	}

	modelsDevModel struct {
		Name             string                  `json:"name"`
		Reasoning        bool                    `json:"reasoning"`
		ReasoningOptions []modelsDevReasoningOpt `json:"reasoning_options"`
		Limit            modelsDevLimit          `json:"limit"`
		Cost             *modelsDevCost          `json:"cost"`
	}

	modelsDevReasoningOpt struct {
		Type   string   `json:"type"`
		Min    int      `json:"min"`
		Values []string `json:"values"`
	}

	modelsDevLimit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	}

	modelsDevCost struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
	}
)

// parseModelsDev converts the api.json payload into section → id → spec.
func parseModelsDev(body []byte) (map[string]map[string]catalog.ModelSpec, error) {
	var raw map[string]modelsDevSection
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse models.dev catalog: %w", err)
	}

	if len(raw) == 0 {
		return nil, errors.New("parse models.dev catalog: no provider sections")
	}

	sections := make(map[string]map[string]catalog.ModelSpec, len(raw))

	for name, section := range raw {
		models := make(map[string]catalog.ModelSpec, len(section.Models))
		for id, m := range section.Models {
			spec := m.toSpec()
			spec.Source = name
			models[id] = spec
		}

		sections[name] = models
	}

	return sections, nil
}

func (m modelsDevModel) toSpec() catalog.ModelSpec {
	spec := catalog.ModelSpec{
		Name:          m.Name,
		ContextWindow: m.Limit.Context,
		MaxTokens:     m.Limit.Output,
		Reasoning:     m.reasoningSpec(),
	}

	if m.Cost != nil {
		spec.Pricing = &config.ModelPricing{
			InputPrice:      m.Cost.Input,
			OutputPrice:     m.Cost.Output,
			CacheReadPrice:  m.Cost.CacheRead,
			CacheWritePrice: m.Cost.CacheWrite,
		}
	}

	return spec
}

func (m modelsDevModel) reasoningSpec() *config.ReasoningSpec {
	spec := &config.ReasoningSpec{Supported: m.Reasoning}
	if !m.Reasoning {
		return spec
	}

	for _, opt := range m.ReasoningOptions {
		switch opt.Type {
		case reasoningTypeEffort:
			spec.NativeEffort = true
			spec.Efforts = catalog.SortEfforts(opt.Values)
		case reasoningTypeBudget:
			spec.BudgetMin = opt.Min
		}
	}

	if !spec.NativeEffort && spec.BudgetMin == 0 {
		spec.BudgetMin = defaultBudgetMin
	}

	return spec
}
