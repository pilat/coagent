package managerdiscovery

import (
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/sessionstore"
)

// Service provides manager-scoped project and configuration discovery.
type Service struct {
	backend *sessionstore.Store
	cfg     *config.Config
	cache   *loader.MarketplaceCache
}

func New(backend *sessionstore.Store, cfg *config.Config, cache *loader.MarketplaceCache) *Service {
	return &Service{backend: backend, cfg: cfg, cache: cache}
}

func (s *Service) unifiedConfig() *config.UnifiedConfig {
	if s.cfg == nil {
		return nil
	}

	return s.cfg.UnifiedConfig
}
