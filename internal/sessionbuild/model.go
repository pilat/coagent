package sessionbuild

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/registry"
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

// BuildClient applies agent policy and validates a model before its record changes.
func BuildClient(
	ctx context.Context,
	in BuildInput,
	model, requested string,
) (llm.Client, sessionprompt.ModelSection, error) {
	if in.Config == nil || in.Config.UnifiedConfig == nil {
		return nil, sessionprompt.ModelSection{}, errors.New("no models configured")
	}

	if in.Record == nil {
		return nil, sessionprompt.ModelSection{}, errors.New("session record is required")
	}

	if _, _, err := prepareClientInput(ctx, &in); err != nil {
		return nil, sessionprompt.ModelSection{}, err
	}

	cfg := in.Config

	level, err := ResolveReasoningLevel(cfg.UnifiedConfig.Models, model, requested)
	if err != nil {
		return nil, sessionprompt.ModelSection{}, err
	}

	client, err := llm.NewClientWithModel(cfg, model)
	if err != nil {
		return nil, sessionprompt.ModelSection{}, fmt.Errorf("build client: %w", err)
	}

	client.SetReasoningLevel(level)

	return client, sessionprompt.ModelSection{
		ID:           model,
		Text:         sessionprompt.BuildModelsSection(model),
		NativeSearch: cfg.UnifiedConfig.SearchNativeActive(model),
	}, nil
}

func prepareClientInput(ctx context.Context, in *BuildInput) (*registry.Set, registry.AgentTypeConfig, error) {
	cfg := *in.Config
	cfg.WorkDir, cfg.RepoRoot = in.WorkDir, in.RepoRoot

	cfg.Model = in.Record.Model
	if cfg.Model == "" {
		cfg.Model = cfg.DefaultModel()
	}

	in.Config = &cfg
	if in.Loader == nil {
		in.Loader = loader.New(in.MarketplaceCache)
	}

	set := registry.NewSet(loadProjectSubagents(ctx, *in, in.WorkDir))

	agentType := registry.AgentType(in.Record.AgentType)
	if agentType == "" {
		agentType = registry.AgentTypeBuild
	}

	agent, ok := set.Get(agentType)
	if !ok {
		return nil, registry.AgentTypeConfig{}, fmt.Errorf("unknown agent type: %s", agentType)
	}

	if agent.DisableIntegratedSearch && cfg.UnifiedConfig != nil {
		unified := *cfg.UnifiedConfig
		disabled := false
		unified.Tools.Search.Enabled = &disabled
		cfg.UnifiedConfig = &unified
	}

	return set, agent, nil
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
