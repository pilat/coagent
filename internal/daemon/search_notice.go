package daemon

import (
	"context"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/logger"
)

// searchUnconfigured reports whether this daemon offers no integrated search
// at all: the tools.search section carries no user choice and no configured
// model sits on a native-capable driver. An explicitly disabled search is a
// choice, not an omission, and stays silent — as does any configured provider.
func searchUnconfigured(unified *config.UnifiedConfig) bool {
	if unified == nil {
		return false // no config yet; onboarding decides what exists
	}

	search := unified.Tools.Search
	if search.SearchActive() || search.SearchDisabled() {
		return false
	}

	for _, m := range unified.Models {
		if unified.SearchNativeActive(m.ID) {
			return false
		}
	}

	return true
}

// noticeSearchUnconfigured logs the one-time discoverability hint. Info, not
// Warn: the absence of search is not a malfunction.
func (s *svc) noticeSearchUnconfigured(ctx context.Context) {
	if !s.searchUnconfigured {
		return
	}

	logger.Ctx(ctx).Named("daemon.search_notice").Info("search_not_configured",
		zap.String("hint",
			"sessions have no web search; set tools.search.provider (tavily or searxng) in config.yaml, "+
				"or use an openrouter-driver model for native search"))
}
