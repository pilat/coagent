package managerdiscovery

import (
	"context"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/loader"
)

type Service interface {
	CreateProject(
		context.Context,
		string,
		controllerapi.ProjectCreateData,
	) (*controllerapi.ProjectCreateResultData, error)
	ListRecentProjects(context.Context) (*controllerapi.ProjectListResultData, error)
	ListDir(context.Context, controllerapi.FsListDirData) (*controllerapi.FsListDirResultData, error)
	ListModels(context.Context) (*controllerapi.ConfigModelsResultData, error)
	ListSkills(
		context.Context,
		string,
		controllerapi.ConfigSkillsData,
	) (*controllerapi.ConfigSkillsResultData, error)
}

var _ Service = (*service)(nil)

type service struct {
	backend Backend
	cfg     *config.Config
	cache   loader.MarketplaceCache
}

func New(backend Backend, cfg *config.Config, cache loader.MarketplaceCache) Service {
	return &service{backend: backend, cfg: cfg, cache: cache}
}

func (s *service) unifiedConfig() *config.UnifiedConfig {
	if s.cfg == nil {
		return nil
	}

	return s.cfg.UnifiedConfig
}
