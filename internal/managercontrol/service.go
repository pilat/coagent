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

func (s *service) unifiedConfig() *config.UnifiedConfig {
	if s.cfg == nil {
		return nil
	}

	return s.cfg.UnifiedConfig
}
