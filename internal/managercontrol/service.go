package managercontrol

import (
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/managerdiscovery"
	"github.com/pilat/coagent/internal/sessionbus"
)

type service struct {
	backend   Backend
	store     Store
	bus       *sessionbus.Bus
	cache     *loader.MarketplaceCache
	discovery *managerdiscovery.Service
}
