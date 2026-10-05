package sessionbuild

import (
	"context"
	"slices"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/tool"
)

func loadMarketplaces(ctx context.Context, p BuildInput, log *zap.Logger) {
	if p.Config.UnifiedConfig == nil {
		log.Info("marketplaces_skipped", zap.String("reason", "unified_config_not_loaded"))
		return
	}

	if len(p.Config.UnifiedConfig.Marketplaces) == 0 {
		log.Info("marketplaces_skipped", zap.String("reason", "no_marketplaces_configured"))
		return
	}

	var resolver loader.RepositoryResolver
	if p.GitClient != nil {
		resolver = loader.NewRepoResolver(p.GitClient)
	}

	log.Info("marketplaces_loading", zap.Int("count", len(p.Config.UnifiedConfig.Marketplaces)))

	p.Loader.ProcessMarketplaces(ctx, p.Config.UnifiedConfig.Marketplaces, resolver)
}

// Resolve the agent type before loading instructions that lean specialists omit.
func loadProjectSubagents(ctx context.Context, p BuildInput, workDir string) []registry.AgentTypeConfig {
	log := logger.Ctx(ctx).Named("session.setup")

	loadMarketplaces(ctx, p, log)

	if err := p.Loader.LoadSubagents(workDir); err != nil {
		log.Warn("loading_subagents", zap.Error(err))
	}

	var models []config.ModelEntry
	if p.Config.UnifiedConfig != nil {
		models = p.Config.UnifiedConfig.Models
	}

	return subagentConfigs(ctx, p.Loader, models)
}

func loadProjectInstructions(ctx context.Context, p BuildInput, workDir string) string {
	log := logger.Ctx(ctx).Named("session.setup")

	agentsMD, err := p.Loader.LoadAgentsMD(workDir)
	if err != nil {
		log.Warn("loading_agents_md", zap.Error(err))
	}

	if err := p.Loader.LoadSkills(workDir); err != nil {
		log.Warn("loading_skills", zap.Error(err))
	}

	return agentsMD
}

// Unknown model overrides fall back to inheritance rather than failing later at spawn.
func subagentConfigs(
	ctx context.Context,
	ldr loader.Service,
	models []config.ModelEntry,
) []registry.AgentTypeConfig {
	subs := ldr.ListSubagents()
	configs := make([]registry.AgentTypeConfig, 0, len(subs))

	for _, sa := range subs {
		if sa.Name == tool.BrowserAgentType {
			logger.Ctx(ctx).Named("session.setup").Warn("subagent_browser_reserved", zap.String("path", sa.Path))
			continue
		}

		model := sa.Model
		if !modelConfigured(models, model) {
			logger.Ctx(ctx).Named("session.setup").Warn(
				"subagent_model_unknown",
				zap.String("subagent", sa.Name),
				zap.String("model", model),
				zap.String("path", sa.Path),
			)

			model = ""
		}

		configs = append(configs, registry.AgentTypeConfig{
			Name:        registry.AgentType(sa.Name),
			Description: sa.Description,
			Mode:        registry.ModeSubagent,
			Tools:       sa.Tools,
			Prompt:      sa.Prompt,
			Model:       model,
		})
	}

	return configs
}

// An absent catalog provides no evidence to reject an override.
func modelConfigured(models []config.ModelEntry, model string) bool {
	if model == "" || len(models) == 0 {
		return true
	}

	return slices.ContainsFunc(models, func(m config.ModelEntry) bool { return m.ID == model })
}
