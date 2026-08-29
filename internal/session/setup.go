package session

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// loadMarketplaces loads marketplace plugins from the unified config.
func loadMarketplaces(ctx context.Context, p params, log *zap.Logger) {
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

// loadProjectContext loads marketplace plugins, AGENTS.md, skills, and subagent
// definitions from the project directory, returning the AGENTS.md text and the
// project-local subagent configs for the session's agent-type set.
func loadProjectContext(ctx context.Context, p params, workDir string) (string, []registry.AgentTypeConfig) {
	log := logger.Ctx(ctx).Named("session.setup")

	// Load marketplaces first
	loadMarketplaces(ctx, p, log)

	agentsMD, err := p.Loader.LoadAgentsMD(workDir)
	if err != nil {
		log.Warn("loading_agents_md", zap.Error(err))
	}

	if err := p.Loader.LoadSkills(workDir); err != nil {
		log.Warn("loading_skills", zap.Error(err))
	}

	if err := p.Loader.LoadSubagents(workDir); err != nil {
		log.Warn("loading_subagents", zap.Error(err))
	}

	var models []config.ModelEntry
	if p.Config.UnifiedConfig != nil {
		models = p.Config.UnifiedConfig.Models
	}

	return agentsMD, subagentConfigs(ctx, p.Loader, models)
}

// subagentConfigs converts project-local subagent definitions into agent-type
// configs. An unknown `model:` is dropped with a warning, not failed at spawn.
func subagentConfigs(
	ctx context.Context,
	ldr loader.Service,
	models []config.ModelEntry,
) []registry.AgentTypeConfig {
	subs := ldr.ListSubagents()
	configs := make([]registry.AgentTypeConfig, 0, len(subs))

	for _, sa := range subs {
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

// modelConfigured reports whether an override is resolvable. No override and no
// catalog both pass — there is nothing to reject in either case.
func modelConfigured(models []config.ModelEntry, model string) bool {
	if model == "" || len(models) == 0 {
		return true
	}

	return slices.ContainsFunc(models, func(m config.ModelEntry) bool { return m.ID == model })
}

// registerSessionTools creates and registers tools that depend on the session.
func registerSessionTools(reg tool.Registry, session *svc) {
	// Curated memory tools (memory_save / memory_delete).
	if session.projectID != 0 && session.memoryStore != nil {
		refreshFn := func(ctx context.Context) {
			session.prompt.refreshMemories(ctx, session.memoryStore, session.projectID)
		}
		reg.Register(builtin.NewMemorySaveTool(session.memoryStore, session.projectID, refreshFn))
		reg.Register(builtin.NewMemoryDeleteTool(session.memoryStore, session.projectID, refreshFn))
	}

	// Subagent tools (task / get_subagent_result / send_to_subagent) are
	// registered by the daemon onto the live registry — they need its spawner.
}

// persistState persists current session metadata to the daemon store.
func (s *svc) persistState(ctx context.Context, iteration int, status sessionstore.SessionStatus) error {
	if s.store == nil {
		return nil
	}

	if err := s.store.UpdateSessionIteration(ctx, s.id, iteration, status); err != nil {
		return fmt.Errorf("update session iteration: %w", err)
	}

	todoItems := s.todoStore.List()
	if len(todoItems) > 0 {
		data, err := json.Marshal(todoItems)
		if err != nil {
			return fmt.Errorf("marshal todo items: %w", err)
		}

		if err := s.store.UpdateSessionTodoItems(ctx, s.id, data); err != nil {
			return fmt.Errorf("update todo items: %w", err)
		}
	}

	return nil
}

func (s *svc) persistErrorState(ctx context.Context, iteration int, content string) error {
	if s.store == nil {
		return nil
	}

	if outputs, ok := s.store.(sessionstore.StateOutputStore); ok && s.outputEnabled {
		if _, err := outputs.UpdateSessionIterationWithOutput(
			ctx, s.id, iteration, sessionstore.SessionStatusError, content,
		); err != nil {
			return fmt.Errorf("update session error with output: %w", err)
		}

		return nil
	}

	return s.persistState(ctx, iteration, sessionstore.SessionStatusError)
}
