package sessionbuild

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/sessionprompt"
)

// ResolveReasoningLevel validates effort against the model catalog and applies its default.
func ResolveReasoningLevel(models []config.ModelEntry, modelID, requested string) (string, error) {
	for _, m := range models {
		if m.ID == modelID {
			return resolveEffort(m, requested)
		}
	}

	return "", fmt.Errorf("unknown model: %s", modelID)
}

// BuildClient validates and constructs a model before its record changes.
func BuildClient(cfg *config.Config, model, requested string) (llm.Client, sessionprompt.ModelSection, error) {
	if cfg == nil || cfg.UnifiedConfig == nil {
		return nil, sessionprompt.ModelSection{}, errors.New("no models configured")
	}
	level, err := ResolveReasoningLevel(cfg.UnifiedConfig.Models, model, requested)
	if err != nil {
		return nil, sessionprompt.ModelSection{}, err
	}
	client, err := llm.NewClientWithModel(cfg, model)
	if err != nil {
		return nil, sessionprompt.ModelSection{}, err
	}
	client.SetReasoningLevel(level)
	return client, sessionprompt.ModelSection{
		ID:           model,
		Text:         sessionprompt.BuildModelsSection(model),
		NativeSearch: cfg.UnifiedConfig.SearchNativeActive(model),
	}, nil
}

// Models offering no effort choice must not receive an unsupported level.
func resolveEffort(m config.ModelEntry, requested string) (string, error) {
	if len(m.EffortLevels) == 0 {
		return "", nil
	}

	if requested == "" {
		return m.DefaultEffort, nil
	}

	if !slices.Contains(m.EffortLevels, requested) {
		return "", fmt.Errorf(
			"model %s does not accept reasoning level %q (accepts: %s)",
			m.ID, requested, strings.Join(m.EffortLevels, ", "),
		)
	}

	return requested, nil
}
