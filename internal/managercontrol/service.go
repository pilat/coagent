package managercontrol

import (
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/managerdiscovery"
	"github.com/pilat/coagent/internal/sessionstore"
)

type service struct {
	backend   Backend
	outputs   sessionstore.ManagerOutputStore
	cfg       *config.Config
	cache     loader.MarketplaceCache
	discovery managerdiscovery.Service
}

func newService(
	backend Backend,
	discoveryBackend managerdiscovery.Backend,
	outputs sessionstore.ManagerOutputStore,
	cfg *config.Config,
	cache loader.MarketplaceCache,
) *service {
	return &service{
		backend: backend, outputs: outputs, cfg: cfg, cache: cache,
		discovery: managerdiscovery.New(discoveryBackend, cfg, cache),
	}
}

func (s *service) unifiedConfig() *config.UnifiedConfig {
	if s.cfg == nil {
		return nil
	}

	return s.cfg.UnifiedConfig
}
