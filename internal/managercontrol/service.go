package managercontrol

import (
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/managerdiscovery"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/sessionbus"
)

type service struct {
	backend   Backend
	store     Store
	progress  progressruntime.Service
	bus       sessionbus.Source
	cfg       *config.Config
	cache     loader.MarketplaceCache
	discovery managerdiscovery.Service
}

func newService(backend Backend, store Store, discovery managerdiscovery.Service,
	progress progressruntime.Service, bus sessionbus.Source,
	cfg *config.Config, cache loader.MarketplaceCache,
) *service {
	return &service{
		backend: backend, store: store, progress: progress, bus: bus, cfg: cfg, cache: cache,
		discovery: discovery,
	}
}

func (s *service) unifiedConfig() *config.UnifiedConfig {
	if s.cfg == nil {
		return nil
	}

	return s.cfg.UnifiedConfig
}
