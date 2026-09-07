package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	validOpenRouterDataCollections = map[string]struct{}{
		"allow": {},
		"deny":  {},
	}
	validOpenRouterQuantizations = map[string]struct{}{
		"int4": {}, "int8": {}, "fp4": {}, "mxfp4": {}, "nvfp4": {}, "fp6": {},
		"fp8": {}, "mxfp8": {}, "fp16": {}, "bf16": {}, "fp32": {}, "unknown": {},
	}
	validOpenRouterSorts = map[string]struct{}{
		"price": {}, "throughput": {}, "latency": {}, "exacto": {},
	}
)

// HasPreferences reports whether any provider preference was configured explicitly.
func (c *OpenRouterConfig) HasPreferences() bool {
	return c != nil && (c.AllowFallbacks != nil || c.DataCollection != "" ||
		c.EnforceDistillableText != nil || len(c.Ignore) > 0 || c.MaxPrice != nil ||
		len(c.Only) > 0 || len(c.Order) > 0 || c.PreferredMaxLatency != nil ||
		c.PreferredMinThroughput != nil || len(c.Quantizations) > 0 ||
		c.RequireParameters != nil || c.Sort != "" || c.ZDR != nil)
}

// UnmarshalYAML accepts OpenRouter's scalar p50 shorthand or percentile object.
func (p *OpenRouterPreference) UnmarshalYAML(node *yaml.Node) error {
	*p = OpenRouterPreference{}

	switch node.Kind {
	case yaml.ScalarNode:
		var value float64
		if err := node.Decode(&value); err != nil {
			return fmt.Errorf("decode scalar OpenRouter preference: %w", err)
		}

		p.Value = &value

		return nil
	case yaml.MappingNode:
		allowed := map[string]struct{}{"p50": {}, "p75": {}, "p90": {}, "p99": {}}

		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i].Value
			if _, ok := allowed[key]; !ok {
				return fmt.Errorf("unknown OpenRouter percentile %q", key)
			}
		}

		type percentiles OpenRouterPreference

		if err := node.Decode((*percentiles)(p)); err != nil {
			return fmt.Errorf("decode OpenRouter percentile preference: %w", err)
		}

		return nil
	case yaml.DocumentNode, yaml.SequenceNode, yaml.AliasNode:
		return errors.New("openrouter preference must be a number or percentile object")
	default:
		return errors.New("openrouter preference has invalid YAML kind")
	}
}

// MarshalYAML preserves the scalar shorthand when one was configured.
func (p OpenRouterPreference) MarshalYAML() (any, error) {
	if p.Value != nil {
		return *p.Value, nil
	}

	type percentiles OpenRouterPreference

	return percentiles(p), nil
}

// MarshalJSON renders the same scalar-or-object union expected by OpenRouter.
func (p OpenRouterPreference) MarshalJSON() ([]byte, error) {
	if p.Value != nil {
		data, err := json.Marshal(*p.Value)
		if err != nil {
			return nil, fmt.Errorf("marshal scalar OpenRouter preference: %w", err)
		}

		return data, nil
	}

	type percentiles OpenRouterPreference

	data, err := json.Marshal(percentiles(p))
	if err != nil {
		return nil, fmt.Errorf("marshal percentile OpenRouter preference: %w", err)
	}

	return data, nil
}

func validateOpenRouterConfig(modelID string, c *OpenRouterConfig) error {
	if c == nil {
		return nil
	}

	if c.Sort != "" {
		if _, ok := validOpenRouterSorts[c.Sort]; !ok {
			return fmt.Errorf(
				"model %q has invalid openrouter_config.sort %q: must be price, throughput, latency, or exacto",
				modelID,
				c.Sort,
			)
		}

		if len(c.Order) > 0 {
			return fmt.Errorf("model %q cannot set both openrouter_config.sort and order", modelID)
		}
	}

	if c.DataCollection != "" {
		if _, ok := validOpenRouterDataCollections[c.DataCollection]; !ok {
			return fmt.Errorf(
				"model %q has invalid openrouter_config.data_collection %q: must be allow or deny",
				modelID,
				c.DataCollection,
			)
		}
	}

	for name, values := range map[string][]string{
		"ignore": c.Ignore,
		"only":   c.Only,
		"order":  c.Order,
	} {
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("model %q has an empty openrouter_config.%s entry", modelID, name)
			}
		}
	}

	for _, value := range c.Quantizations {
		if _, ok := validOpenRouterQuantizations[value]; !ok {
			return fmt.Errorf(
				"model %q has invalid openrouter_config.quantizations entry %q",
				modelID,
				value,
			)
		}
	}

	if err := validateOpenRouterPreference("preferred_max_latency", c.PreferredMaxLatency); err != nil {
		return fmt.Errorf("model %q: %w", modelID, err)
	}

	if err := validateOpenRouterPreference("preferred_min_throughput", c.PreferredMinThroughput); err != nil {
		return fmt.Errorf("model %q: %w", modelID, err)
	}

	if err := validateOpenRouterMaxPrice(c.MaxPrice); err != nil {
		return fmt.Errorf("model %q: %w", modelID, err)
	}

	return nil
}

func validateOpenRouterPreference(name string, p *OpenRouterPreference) error {
	if p == nil {
		return nil
	}

	percentiles := []*float64{p.P50, p.P75, p.P90, p.P99}
	if p.Value != nil && anyNonNil(percentiles) {
		return fmt.Errorf("openrouter_config.%s cannot mix a scalar with percentile cutoffs", name)
	}

	if p.Value == nil && !anyNonNil(percentiles) {
		return fmt.Errorf("openrouter_config.%s must set a scalar or at least one percentile", name)
	}

	values := percentiles
	if p.Value != nil {
		values = []*float64{p.Value}
	}

	for _, value := range values {
		if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return fmt.Errorf("openrouter_config.%s values must be finite and non-negative", name)
		}
	}

	return nil
}

func validateOpenRouterMaxPrice(price *OpenRouterMaxPrice) error {
	if price == nil {
		return nil
	}

	values := []*float64{price.Audio, price.Completion, price.Image, price.Prompt, price.Request}
	if !anyNonNil(values) {
		return errors.New("openrouter_config.max_price must set at least one price")
	}

	for _, value := range values {
		if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return errors.New("openrouter_config.max_price values must be finite and non-negative")
		}
	}

	return nil
}

func anyNonNil[T any](values []*T) bool {
	for _, value := range values {
		if value != nil {
			return true
		}
	}

	return false
}
